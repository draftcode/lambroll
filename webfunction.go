package lambroll

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Songmu/prompter"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdaweb"
	webtypes "github.com/aws/aws-sdk-go-v2/service/lambdaweb/types"
	"github.com/samber/lo"
)

// WebFunction represents configuration of Lambda Web Function
type WebFunction lambdaweb.CreateWebFunctionInput

var DefaultWebFunctionFilenames = []string{
	"web_function.json",
	"web_function.jsonnet",
}

// WebDeployOption represents an option for WebDeploy()
type WebDeployOption struct {
	Src         string        `help:"function zip archive or src dir" default:"." json:"src,omitempty"`
	SkipArchive bool          `help:"skip to create zip archive and use the existing S3 object" default:"false" json:"skip_archive,omitempty"`
	WaitTimeout time.Duration `help:"timeout to wait for the revision and the endpoint to become active" default:"10m" json:"wait_timeout,omitempty"`

	DryRunOption
	ZipOption
}

// WebDeleteOption represents an option for WebDelete()
type WebDeleteOption struct {
	Force bool `help:"delete without confirmation" default:"false" json:"force,omitempty"`

	DryRunOption
}

func (app *App) loadWebFunction(path string) (*WebFunction, error) {
	return loadDefinitionFile[WebFunction](app, path, DefaultWebFunctionFilenames)
}

// WebDeploy creates a Web Function, or deploys a new revision of an existing one
func (app *App) WebDeploy(ctx context.Context, opt *WebDeployOption) error {
	logger := opt.logger()
	if err := opt.Expand(); err != nil {
		return err
	}
	fn, err := app.loadWebFunction(app.functionFilePath)
	if err != nil {
		return fmt.Errorf("failed to load web function: %w", err)
	}
	rc := fn.RevisionConfig
	if rc == nil || rc.BuildConfig == nil || rc.BuildConfig.CodeConfig == nil || rc.BuildConfig.CodeConfig.S3Object == nil {
		return fmt.Errorf("RevisionConfig.BuildConfig.CodeConfig.S3Object is required in web function definition")
	}
	obj := rc.BuildConfig.CodeConfig.S3Object

	logger.Info("starting deploy web function", "function", *fn.FunctionName)
	if opt.DryRun {
		return nil
	}

	if !opt.SkipArchive {
		zipfile, info, err := prepareZipfile(opt.Src, opt.excludes, opt.KeepSymlink)
		if err != nil {
			return err
		}
		defer zipfile.Close()
		logger.Info("uploading function to s3", "bytes", info.Size(), "bucket", *obj.Bucket, "key", *obj.Key)
		versionID, err := app.uploadFunctionToS3(ctx, zipfile, *obj.Bucket, *obj.Key)
		if err != nil {
			return fmt.Errorf("failed to upload function zip to s3://%s/%s: %w", *obj.Bucket, *obj.Key, err)
		}
		if versionID == "" {
			return fmt.Errorf("s3://%s has no versioning enabled, which Web Functions require", *obj.Bucket)
		}
		obj.VersionId = aws.String(versionID)
	}

	var revisionID string
	if _, err := app.lambdaweb.GetWebFunction(ctx, &lambdaweb.GetWebFunctionInput{
		FunctionName: fn.FunctionName,
	}); err != nil {
		if !isWebNotFound(err) {
			return fmt.Errorf("failed to get web function: %w", err)
		}
		in := lambdaweb.CreateWebFunctionInput(*fn)
		res, err := app.lambdaweb.CreateWebFunction(ctx, &in)
		if err != nil {
			return fmt.Errorf("failed to create web function: %w", err)
		}
		revisionID = aws.ToString(res.Revision.RevisionId)
		logger.Info("created web function", "function", *fn.FunctionName, "revision", revisionID)
	} else {
		if err := app.updateWebFunctionTags(ctx, opt, fn); err != nil {
			return err
		}
		res, err := app.lambdaweb.CreateWebFunctionRevision(ctx, &lambdaweb.CreateWebFunctionRevisionInput{
			FunctionName:  fn.FunctionName,
			BuildConfig:   rc.BuildConfig,
			ServiceConfig: rc.ServiceConfig,
			Description:   rc.Description,
			KmsKeyArn:     rc.KmsKeyArn,
		})
		if err != nil {
			return fmt.Errorf("failed to create web function revision: %w", err)
		}
		revisionID = aws.ToString(res.RevisionId)
		logger.Info("created revision", "function", *fn.FunctionName, "revision", revisionID)
	}

	logger.Info("waiting for revision to become active", "revision", revisionID)
	if err := lambdaweb.NewWebFunctionRevisionActiveWaiter(app.lambdaweb).Wait(ctx, &lambdaweb.GetWebFunctionRevisionInput{
		FunctionName: fn.FunctionName,
		RevisionId:   aws.String(revisionID),
	}, opt.WaitTimeout); err != nil {
		return fmt.Errorf("revision %s did not become active: %w", revisionID, err)
	}

	if fn.EndpointConfig == nil {
		logger.Info("completed to deploy web function", "function", *fn.FunctionName, "revision", revisionID)
		return nil
	}
	return app.deployWebFunctionEndpoint(ctx, opt, fn, revisionID)
}

// deployWebFunctionEndpoint creates or updates the endpoint to match the definition and routes it to the revision
func (app *App) deployWebFunctionEndpoint(ctx context.Context, opt *WebDeployOption, fn *WebFunction, revisionID string) error {
	logger := opt.logger()
	ec := fn.EndpointConfig
	var weights []webtypes.RevisionWeight
	if ec.AutoDeploymentMode != webtypes.AutoDeploymentModeLatestRevision {
		weights = []webtypes.RevisionWeight{{RevisionId: aws.String(revisionID), Weight: aws.Int32(100)}}
	}
	epIn := &lambdaweb.GetWebFunctionEndpointInput{FunctionName: fn.FunctionName, EndpointName: ec.EndpointName}

	ep, err := app.lambdaweb.GetWebFunctionEndpoint(ctx, epIn)
	if err != nil {
		if !isWebNotFound(err) {
			return fmt.Errorf("failed to get web function endpoint: %w", err)
		}
		logger.Info("creating endpoint", "endpoint", *ec.EndpointName)
		if _, err := app.lambdaweb.CreateWebFunctionEndpoint(ctx, &lambdaweb.CreateWebFunctionEndpointInput{
			FunctionName:       fn.FunctionName,
			EndpointName:       ec.EndpointName,
			EndpointType:       ec.EndpointType,
			AuthType:           ec.AuthType,
			AutoDeploymentMode: ec.AutoDeploymentMode,
			Description:        ec.Description,
			Regions:            ec.Regions,
			ScalingConfig:      ec.ScalingConfig,
			ThrottleConfig:     ec.ThrottleConfig,
			RevisionWeights:    weights,
		}); err != nil {
			return fmt.Errorf("failed to create web function endpoint: %w", err)
		}
	} else {
		if ec.EndpointType != "" && ec.EndpointType != ep.EndpointType {
			return fmt.Errorf("EndpointType of endpoint %s cannot be changed from %s to %s", *ec.EndpointName, ep.EndpointType, ec.EndpointType)
		}
		if ec.Regions != nil && !slices.Equal(sortedCopy(ec.Regions), sortedCopy(ep.Regions)) {
			return fmt.Errorf("Regions of endpoint %s cannot be changed", *ec.EndpointName)
		}
		if err := lambdaweb.NewWebFunctionEndpointActiveWaiter(app.lambdaweb).Wait(ctx, epIn, opt.WaitTimeout); err != nil {
			return fmt.Errorf("endpoint %s did not become active: %w", *ec.EndpointName, err)
		}
		logger.Info("updating endpoint", "endpoint", *ec.EndpointName, "revision", revisionID)
		if _, err := app.lambdaweb.UpdateWebFunctionEndpoint(ctx, &lambdaweb.UpdateWebFunctionEndpointInput{
			FunctionName:       fn.FunctionName,
			EndpointName:       ec.EndpointName,
			AuthType:           ec.AuthType,
			AutoDeploymentMode: ec.AutoDeploymentMode,
			Description:        ec.Description,
			ScalingConfig:      ec.ScalingConfig,
			ThrottleConfig:     ec.ThrottleConfig,
			RevisionWeights:    weights,
		}); err != nil {
			return fmt.Errorf("failed to update web function endpoint: %w", err)
		}
		if err := lambdaweb.NewWebFunctionEndpointUpdatedWaiter(app.lambdaweb).Wait(ctx, epIn, opt.WaitTimeout); err != nil {
			return fmt.Errorf("endpoint %s update did not complete: %w", *ec.EndpointName, err)
		}
	}

	ep, err = lambdaweb.NewWebFunctionEndpointActiveWaiter(app.lambdaweb).WaitForOutput(ctx, epIn, opt.WaitTimeout)
	if err != nil {
		return fmt.Errorf("endpoint %s did not become active: %w", *ec.EndpointName, err)
	}
	logger.Info("completed to deploy web function", "function", *fn.FunctionName, "revision", revisionID, "url", "https://"+aws.ToString(ep.DomainName))
	return nil
}

func (app *App) updateWebFunctionTags(ctx context.Context, opt *WebDeployOption, fn *WebFunction) error {
	if fn.Tags == nil {
		return nil
	}
	arn, err := app.webFunctionArn(ctx, *fn.FunctionName)
	if err != nil {
		return err
	}
	current, err := app.lambdaweb.ListTags(ctx, &lambdaweb.ListTagsInput{Resource: aws.String(arn)})
	if err != nil {
		return fmt.Errorf("failed to list tags of %s: %w", arn, err)
	}
	setTags, removeTagKeys := mergeTags(current.Tags, fn.Tags)
	setTags = lo.OmitBy(setTags, func(tag string, _ string) bool { return isAWSManagedTag(tag) })
	removeTagKeys = lo.Reject(removeTagKeys, func(tag string, _ int) bool { return isAWSManagedTag(tag) })
	if len(setTags) > 0 {
		opt.logger().Info("setting tags", "count", len(setTags))
		if _, err := app.lambdaweb.TagResource(ctx, &lambdaweb.TagResourceInput{Resource: aws.String(arn), Tags: setTags}); err != nil {
			return fmt.Errorf("failed to tag resource: %w", err)
		}
	}
	if len(removeTagKeys) > 0 {
		opt.logger().Info("removing tags", "count", len(removeTagKeys))
		if _, err := app.lambdaweb.UntagResource(ctx, &lambdaweb.UntagResourceInput{Resource: aws.String(arn), TagKeys: removeTagKeys}); err != nil {
			return fmt.Errorf("failed to untag resource: %w", err)
		}
	}
	return nil
}

func (app *App) webFunctionArn(ctx context.Context, name string) (string, error) {
	res, err := app.lambdaweb.GetWebFunction(ctx, &lambdaweb.GetWebFunctionInput{FunctionName: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("failed to get web function: %w", err)
	}
	return aws.ToString(res.FunctionArn), nil
}

// remoteWebFunction returns the deployed definition built from the revision serving the endpoint, or nil if the Web Function does not exist
func (app *App) remoteWebFunction(ctx context.Context, name string, endpointName *string) (*WebFunction, error) {
	res, err := app.lambdaweb.GetWebFunction(ctx, &lambdaweb.GetWebFunctionInput{FunctionName: aws.String(name)})
	if err != nil {
		if isWebNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get web function: %w", err)
	}
	fn := &WebFunction{FunctionName: aws.String(name)}

	tags, err := app.lambdaweb.ListTags(ctx, &lambdaweb.ListTagsInput{Resource: res.FunctionArn})
	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}
	if len(tags.Tags) > 0 {
		fn.Tags = tags.Tags
	}

	var weights []webtypes.RevisionWeight
	if endpointName != nil {
		ep, err := app.lambdaweb.GetWebFunctionEndpoint(ctx, &lambdaweb.GetWebFunctionEndpointInput{
			FunctionName: aws.String(name),
			EndpointName: endpointName,
		})
		if err != nil && !isWebNotFound(err) {
			return nil, fmt.Errorf("failed to get web function endpoint: %w", err)
		}
		if ep != nil {
			weights = ep.RevisionWeights
			fn.EndpointConfig = &webtypes.EndpointConfig{
				EndpointName:       ep.EndpointName,
				EndpointType:       ep.EndpointType,
				AuthType:           ep.AuthType,
				AutoDeploymentMode: ep.AutoDeploymentMode,
				Description:        ep.Description,
				Regions:            ep.Regions,
				ScalingConfig:      ep.ScalingConfig,
				ThrottleConfig:     ep.ThrottleConfig,
			}
		}
	}

	revisionID, err := app.servingRevisionID(ctx, name, weights)
	if err != nil || revisionID == "" {
		return fn, err
	}
	rev, err := app.lambdaweb.GetWebFunctionRevision(ctx, &lambdaweb.GetWebFunctionRevisionInput{
		FunctionName: aws.String(name),
		RevisionId:   aws.String(revisionID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get web function revision %s: %w", revisionID, err)
	}
	fn.RevisionConfig = &webtypes.RevisionConfig{
		BuildConfig:   rev.BuildConfig,
		ServiceConfig: rev.ServiceConfig,
		Description:   rev.Description,
		KmsKeyArn:     rev.KmsKeyArn,
	}
	return fn, nil
}

// servingRevisionID returns the revision with the largest weight, or the newest revision when no weights are set
func (app *App) servingRevisionID(ctx context.Context, name string, weights []webtypes.RevisionWeight) (string, error) {
	if len(weights) > 0 {
		w := lo.MaxBy(weights, func(a, b webtypes.RevisionWeight) bool { return aws.ToInt32(a.Weight) > aws.ToInt32(b.Weight) })
		return aws.ToString(w.RevisionId), nil
	}
	res, err := app.lambdaweb.ListWebFunctionRevisions(ctx, &lambdaweb.ListWebFunctionRevisionsInput{
		FunctionName: aws.String(name),
		MaxResults:   aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("failed to list web function revisions: %w", err)
	}
	if len(res.Revisions) == 0 {
		return "", nil
	}
	return aws.ToString(res.Revisions[0].RevisionId), nil
}

func isWebNotFound(err error) bool {
	var nfe *webtypes.ResourceNotFoundException
	return errors.As(err, &nfe)
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// WebDelete deletes a Web Function with its revisions and endpoints
func (app *App) WebDelete(ctx context.Context, opt *WebDeleteOption) error {
	logger := opt.logger()
	fn, err := app.loadWebFunction(app.functionFilePath)
	if err != nil {
		return fmt.Errorf("failed to load web function: %w", err)
	}
	logger.Info("deleting web function", "function", *fn.FunctionName)
	if opt.DryRun {
		return nil
	}
	if !opt.Force && !prompter.YN("Do you want to delete the web function?", false) {
		logger.Info("canceled to delete web function", "function", *fn.FunctionName)
		return nil
	}
	if _, err := app.lambdaweb.DeleteWebFunction(ctx, &lambdaweb.DeleteWebFunctionInput{
		FunctionName: fn.FunctionName,
	}); err != nil {
		return fmt.Errorf("failed to delete web function: %w", err)
	}
	logger.Info("completed to delete web function", "function", *fn.FunctionName)
	return nil
}
