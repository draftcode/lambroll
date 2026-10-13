package lambroll

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdaweb"
	webtypes "github.com/aws/aws-sdk-go-v2/service/lambdaweb/types"
	"github.com/samber/lo"
)

// WebRollbackOption represents options for WebRollback()
type WebRollbackOption struct {
	Revision       string        `default:"" help:"revision to rollback (default: previous active revision auto detected)" json:"revision,omitempty"`
	DeleteRevision bool          `default:"false" help:"delete rolled back revisions" json:"delete_revision,omitempty"`
	WaitTimeout    time.Duration `help:"timeout to wait for the endpoint update" default:"10m" json:"wait_timeout,omitempty"`

	DryRunOption
}

// WebRollback routes all traffic of the endpoint to the previous revision
func (app *App) WebRollback(ctx context.Context, opt *WebRollbackOption) error {
	logger := opt.logger()
	fn, err := app.loadWebFunction(app.functionFilePath)
	if err != nil {
		return fmt.Errorf("failed to load web function: %w", err)
	}
	if fn.EndpointConfig == nil || fn.EndpointConfig.EndpointName == nil {
		return fmt.Errorf("EndpointConfig.EndpointName is required in web function definition to rollback")
	}
	epIn := &lambdaweb.GetWebFunctionEndpointInput{FunctionName: fn.FunctionName, EndpointName: fn.EndpointConfig.EndpointName}
	ep, err := app.lambdaweb.GetWebFunctionEndpoint(ctx, epIn)
	if err != nil {
		return fmt.Errorf("failed to get web function endpoint: %w", err)
	}

	current := lo.Map(ep.RevisionWeights, func(w webtypes.RevisionWeight, _ int) string { return aws.ToString(w.RevisionId) })
	if len(current) == 0 {
		id, err := app.servingRevisionID(ctx, *fn.FunctionName, nil)
		if err != nil {
			return err
		}
		current = []string{id}
	}
	target := opt.Revision
	if target == "" {
		if target, err = app.findPreviousRevision(ctx, *fn.FunctionName, current); err != nil {
			return err
		}
	}

	logger.Info("rolling back web function", "function", *fn.FunctionName, "endpoint", *ep.EndpointName, "from", current, "to", target)
	if opt.DryRun {
		return nil
	}
	if _, err := app.lambdaweb.UpdateWebFunctionEndpoint(ctx, &lambdaweb.UpdateWebFunctionEndpointInput{
		FunctionName:       fn.FunctionName,
		EndpointName:       ep.EndpointName,
		AutoDeploymentMode: webtypes.AutoDeploymentModeDisabled,
		RevisionWeights:    []webtypes.RevisionWeight{{RevisionId: aws.String(target), Weight: aws.Int32(100)}},
	}); err != nil {
		return fmt.Errorf("failed to update web function endpoint: %w", err)
	}
	if err := lambdaweb.NewWebFunctionEndpointUpdatedWaiter(app.lambdaweb).Wait(ctx, epIn, opt.WaitTimeout); err != nil {
		return fmt.Errorf("endpoint %s update did not complete: %w", *ep.EndpointName, err)
	}
	if ep.AutoDeploymentMode == webtypes.AutoDeploymentModeLatestRevision {
		logger.Warn("endpoint auto deployment is disabled by rollback. the next web-deploy restores it from the definition", "endpoint", *ep.EndpointName)
	}

	if !opt.DeleteRevision {
		return nil
	}
	for _, id := range current {
		if id == target {
			continue
		}
		logger.Info("deleting revision", "revision", id)
		if _, err := app.lambdaweb.DeleteWebFunctionRevision(ctx, &lambdaweb.DeleteWebFunctionRevisionInput{
			FunctionName: fn.FunctionName,
			RevisionId:   aws.String(id),
		}); err != nil {
			return fmt.Errorf("failed to delete revision %s: %w", id, err)
		}
	}
	return nil
}

func (app *App) findPreviousRevision(ctx context.Context, name string, current []string) (string, error) {
	var revs []webtypes.FunctionRevisionSummary
	p := lambdaweb.NewListWebFunctionRevisionsPaginator(app.lambdaweb, &lambdaweb.ListWebFunctionRevisionsInput{
		FunctionName: aws.String(name),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list web function revisions: %w", err)
		}
		revs = append(revs, page.Revisions...)
	}
	if id := previousRevision(revs, current); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("unable to detect previous revision of web function")
}

// previousRevision returns the newest active revision older than the current ones, from revisions listed newest first
func previousRevision(revs []webtypes.FunctionRevisionSummary, current []string) string {
	seenCurrent := false
	for _, rev := range revs {
		id := aws.ToString(rev.RevisionId)
		if lo.Contains(current, id) {
			seenCurrent = true
			continue
		}
		if seenCurrent && rev.State == webtypes.RevisionStateActive {
			return id
		}
	}
	return ""
}
