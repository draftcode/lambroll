package lambroll

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aereal/jsondiff"
	"github.com/aws/aws-sdk-go-v2/aws"
	webtypes "github.com/aws/aws-sdk-go-v2/service/lambdaweb/types"
)

// WebDiffOption represents options for WebDiff()
type WebDiffOption struct {
	Ignore   string   `help:"ignore diff by jq query" default:"" json:"ignore,omitempty"`
	ExitCode bool     `help:"exit with code 2 if there are differences" default:"false" json:"exit_code,omitempty"`
	External string   `help:"external command to display diff" default:"" env:"LAMBROLL_DIFF_COMMAND" json:"external,omitempty"`
	Mask     []string `help:"mask values in diff output by jq selector or environment variable name (repeatable)" json:"mask,omitempty"`
}

// WebDiff prints diff of web_function.json compared with the revision serving the endpoint
func (app *App) WebDiff(ctx context.Context, opt *WebDiffOption) error {
	fn, err := app.loadWebFunction(app.functionFilePath)
	if err != nil {
		return fmt.Errorf("failed to load web function: %w", err)
	}
	name := aws.ToString(fn.FunctionName)
	var endpointName *string
	if fn.EndpointConfig != nil {
		endpointName = fn.EndpointConfig.EndpointName
	}
	remote, err := app.remoteWebFunction(ctx, name, endpointName)
	if err != nil {
		return err
	}
	if remote == nil {
		slog.Info("web function not found. lambroll web-deploy will create a new web function", "function", name)
	}
	fillWebFunctionDefaults(fn)
	normalizeWebFunctionForDiff(fn, remote)

	remoteJSON, _ := marshalAny(remote)
	localJSON, _ := marshalAny(fn)
	hasDiff, err := app.emitDiff(ctx, &DiffOption{External: opt.External, w: os.Stdout}, "web_function.json",
		&jsondiff.Input{Name: name, X: remoteJSON},
		&jsondiff.Input{Name: app.functionFilePath, X: localJSON},
		opt.Ignore, resolveMaskSelectors(opt.Mask),
	)
	if err != nil {
		return err
	}
	if hasDiff && opt.ExitCode {
		return ErrDiff
	}
	return nil
}

func fillWebFunctionDefaults(fn *WebFunction) {
	if fn.RevisionConfig == nil {
		return
	}
	if fn.RevisionConfig.ServiceConfig == nil {
		fn.RevisionConfig.ServiceConfig = &webtypes.ServiceConfig{}
	}
	if fn.RevisionConfig.ServiceConfig.TimeoutSeconds == nil {
		fn.RevisionConfig.ServiceConfig.TimeoutSeconds = aws.Int32(30)
	}
}

// normalizeWebFunctionForDiff drops the remote S3 object version when the local definition does not pin it, because web-deploy resolves it after uploading
func normalizeWebFunctionForDiff(local, remote *WebFunction) {
	if remote == nil || remote.RevisionConfig == nil || remote.RevisionConfig.BuildConfig == nil || remote.RevisionConfig.BuildConfig.CodeConfig == nil {
		return
	}
	obj := remote.RevisionConfig.BuildConfig.CodeConfig.S3Object
	if obj == nil {
		return
	}
	lrc := local.RevisionConfig
	if lrc == nil || lrc.BuildConfig == nil || lrc.BuildConfig.CodeConfig == nil || lrc.BuildConfig.CodeConfig.S3Object == nil || lrc.BuildConfig.CodeConfig.S3Object.VersionId == nil {
		obj.VersionId = nil
	}
}
