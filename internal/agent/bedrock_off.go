//go:build !bedrock

package agent

import (
	"errors"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/usage"
)

// newBedrock: default builds carry no AWS SDK (D-010).
func newBedrock(string) (usage.Poller, error) {
	return nil, errors.New("agent: usage.bedrock is enabled but this build has no Bedrock support; use the plumbline-agent build made with -tags bedrock")
}
