package lambroll

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	webtypes "github.com/aws/aws-sdk-go-v2/service/lambdaweb/types"
)

func TestPreviousRevision(t *testing.T) {
	rev := func(id string, state webtypes.RevisionState) webtypes.FunctionRevisionSummary {
		return webtypes.FunctionRevisionSummary{RevisionId: aws.String(id), State: state}
	}
	active, failed := webtypes.RevisionStateActive, webtypes.RevisionStateFailed
	revs := []webtypes.FunctionRevisionSummary{
		rev("r5", active), rev("r4", active), rev("r3", failed), rev("r2", active), rev("r1", active),
	}
	cases := []struct {
		name    string
		current []string
		want    string
	}{
		{"skips failed revision", []string{"r4"}, "r2"},
		{"ignores newer revisions", []string{"r2"}, "r1"},
		{"canary rolls back past both", []string{"r5", "r4"}, "r2"},
		{"no older revision", []string{"r1"}, ""},
		{"unknown current", []string{"rX"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := previousRevision(revs, c.current); got != c.want {
				t.Errorf("previousRevision(%v) = %q, want %q", c.current, got, c.want)
			}
		})
	}
}
