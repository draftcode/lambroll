package lambroll

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdaweb"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

// WebStatusOption represents options for WebStatus()
type WebStatusOption struct {
	Output string `help:"output format" default:"table" enum:"table,json" json:"output,omitempty"`
}

type WebStatusOutput struct {
	FunctionName   string              `json:"FunctionName"`
	FunctionArn    string              `json:"FunctionArn"`
	State          string              `json:"State"`
	LatestRevision string              `json:"LatestRevision,omitempty"`
	Endpoints      []WebEndpointStatus `json:"Endpoints"`
}

type WebEndpointStatus struct {
	EndpointName       string           `json:"EndpointName"`
	URL                string           `json:"URL"`
	EndpointType       string           `json:"EndpointType"`
	AutoDeploymentMode string           `json:"AutoDeploymentMode"`
	State              string           `json:"State"`
	UpdateStatus       string           `json:"UpdateStatus"`
	RevisionWeights    map[string]int32 `json:"RevisionWeights,omitempty"`
}

func (o *WebStatusOutput) String() string {
	buf := new(strings.Builder)
	w := tablewriter.NewTable(buf, tablewriter.WithRendition(tw.Rendition{
		Symbols: tw.NewSymbols(tw.StyleASCII),
	}))
	w.Append("FunctionName", o.FunctionName)
	w.Append("FunctionArn", o.FunctionArn)
	w.Append("State", o.State)
	if o.LatestRevision != "" {
		w.Append("LatestRevision", o.LatestRevision)
	}
	for _, ep := range o.Endpoints {
		w.Append("Endpoint", ep.EndpointName)
		w.Append("  URL", ep.URL)
		w.Append("  EndpointType", ep.EndpointType)
		w.Append("  AutoDeploymentMode", ep.AutoDeploymentMode)
		w.Append("  State", ep.State)
		w.Append("  UpdateStatus", ep.UpdateStatus)
		for id, weight := range ep.RevisionWeights {
			w.Append("  RevisionWeight", fmt.Sprintf("%s: %d", id, weight))
		}
	}
	w.Render()
	return buf.String()
}

// WebStatus prints status of web function and its endpoints
func (app *App) WebStatus(ctx context.Context, opt *WebStatusOption) error {
	fn, err := app.loadWebFunction(app.functionFilePath)
	if err != nil {
		return fmt.Errorf("failed to load web function: %w", err)
	}
	res, err := app.lambdaweb.GetWebFunction(ctx, &lambdaweb.GetWebFunctionInput{FunctionName: fn.FunctionName})
	if err != nil {
		return fmt.Errorf("failed to get web function %s: %w", *fn.FunctionName, err)
	}
	out := &WebStatusOutput{
		FunctionName: aws.ToString(res.FunctionName),
		FunctionArn:  aws.ToString(res.FunctionArn),
		State:        string(res.State),
	}

	revs, err := app.lambdaweb.ListWebFunctionRevisions(ctx, &lambdaweb.ListWebFunctionRevisionsInput{
		FunctionName: fn.FunctionName,
		MaxResults:   aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("failed to list web function revisions: %w", err)
	}
	if len(revs.Revisions) > 0 {
		r := revs.Revisions[0]
		out.LatestRevision = fmt.Sprintf("%s (%s)", aws.ToString(r.RevisionId), r.State)
	}

	p := lambdaweb.NewListWebFunctionEndpointsPaginator(app.lambdaweb, &lambdaweb.ListWebFunctionEndpointsInput{
		FunctionName: fn.FunctionName,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list web function endpoints: %w", err)
		}
		for _, ep := range page.Endpoints {
			s := WebEndpointStatus{
				EndpointName:       aws.ToString(ep.EndpointName),
				URL:                "https://" + aws.ToString(ep.DomainName),
				EndpointType:       string(ep.EndpointType),
				AutoDeploymentMode: string(ep.AutoDeploymentMode),
				State:              string(ep.State),
				UpdateStatus:       string(ep.UpdateStatus),
			}
			if len(ep.RevisionWeights) > 0 {
				s.RevisionWeights = make(map[string]int32)
				for _, rw := range ep.RevisionWeights {
					s.RevisionWeights[aws.ToString(rw.RevisionId)] = aws.ToInt32(rw.Weight)
				}
			}
			out.Endpoints = append(out.Endpoints, s)
		}
	}

	switch opt.Output {
	case "table":
		fmt.Print(out.String())
	case "json":
		b, _ := marshalJSON(out)
		fmt.Print(string(b))
	}
	return nil
}
