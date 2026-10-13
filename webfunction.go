package lambroll

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Songmu/prompter"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdaweb"
	webtypes "github.com/aws/aws-sdk-go-v2/service/lambdaweb/types"
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
	WaitTimeout time.Duration `help:"timeout to wait for the revision to become active" default:"10m" json:"wait_timeout,omitempty"`

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
		var nfe *webtypes.ResourceNotFoundException
		if !errors.As(err, &nfe) {
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
	return app.routeWebFunctionEndpoint(ctx, opt, fn, revisionID)
}

// routeWebFunctionEndpoint sends all traffic of the endpoint to the revision when the endpoint does not auto-deploy
func (app *App) routeWebFunctionEndpoint(ctx context.Context, opt *WebDeployOption, fn *WebFunction, revisionID string) error {
	logger := opt.logger()
	ep, err := app.lambdaweb.GetWebFunctionEndpoint(ctx, &lambdaweb.GetWebFunctionEndpointInput{
		FunctionName: fn.FunctionName,
		EndpointName: fn.EndpointConfig.EndpointName,
	})
	if err != nil {
		return fmt.Errorf("failed to get web function endpoint: %w", err)
	}
	if ep.AutoDeploymentMode != webtypes.AutoDeploymentModeLatestRevision {
		logger.Info("routing endpoint to revision", "endpoint", *ep.EndpointName, "revision", revisionID)
		if _, err := app.lambdaweb.UpdateWebFunctionEndpoint(ctx, &lambdaweb.UpdateWebFunctionEndpointInput{
			FunctionName: fn.FunctionName,
			EndpointName: ep.EndpointName,
			RevisionWeights: []webtypes.RevisionWeight{
				{RevisionId: aws.String(revisionID), Weight: aws.Int32(100)},
			},
		}); err != nil {
			return fmt.Errorf("failed to update web function endpoint: %w", err)
		}
	}
	logger.Info("completed to deploy web function", "function", *fn.FunctionName, "revision", revisionID, "url", "https://"+aws.ToString(ep.DomainName))
	return nil
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
