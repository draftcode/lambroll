package lambroll

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdaweb"
)

// WebInitOption represents options for WebInit()
type WebInitOption struct {
	FunctionName   *string `help:"Web function name for init" required:"true" default:"" json:"function_name,omitempty"`
	EndpointName   *string `help:"endpoint to include in the definition (default: the only endpoint of the web function)" json:"endpoint_name,omitempty"`
	Jsonnet        bool    `help:"render web_function.json as jsonnet" default:"false" json:"jsonnet,omitempty"`
	ForceOverwrite bool    `help:"Overwrite existing files without prompting" default:"false" json:"force_overwrite,omitempty"`
}

// WebInit initializes web_function.json from an existing web function
func (app *App) WebInit(ctx context.Context, opt *WebInitOption) error {
	name := *opt.FunctionName
	endpointName := opt.EndpointName
	if endpointName == nil {
		res, err := app.lambdaweb.ListWebFunctionEndpoints(ctx, &lambdaweb.ListWebFunctionEndpointsInput{
			FunctionName: aws.String(name),
		})
		if err != nil && !isWebNotFound(err) {
			return fmt.Errorf("failed to list web function endpoints: %w", err)
		}
		if res != nil {
			switch len(res.Endpoints) {
			case 0:
			case 1:
				endpointName = res.Endpoints[0].EndpointName
			default:
				return fmt.Errorf("web function %s has multiple endpoints. specify one with --endpoint-name", name)
			}
		}
	}

	fn, err := app.remoteWebFunction(ctx, name, endpointName)
	if err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("web function %s not found", name)
	}
	if rc := fn.RevisionConfig; rc != nil && rc.BuildConfig != nil && rc.BuildConfig.CodeConfig != nil && rc.BuildConfig.CodeConfig.S3Object != nil {
		// a pinned version would make every later deploy with --skip-archive reuse this object
		rc.BuildConfig.CodeConfig.S3Object.VersionId = nil
	}

	slog.Info("creating file", "name", IgnoreFilename)
	if err := app.saveFile(ctx, IgnoreFilename, []byte(strings.Join(DefaultExcludes, "\n")+"\n"), os.FileMode(0644), opt.ForceOverwrite); err != nil {
		return err
	}

	filename := DefaultWebFunctionFilenames[0]
	if opt.Jsonnet {
		filename = DefaultWebFunctionFilenames[1]
	}
	slog.Info("creating file", "name", filename)
	b, _ := marshalJSON(fn)
	if opt.Jsonnet {
		if b, err = jsonToJsonnet(b, filename); err != nil {
			return err
		}
	}
	return app.saveFile(ctx, filename, b, os.FileMode(0644), opt.ForceOverwrite)
}
