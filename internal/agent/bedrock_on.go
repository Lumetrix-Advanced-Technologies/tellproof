//go:build bedrock

package agent

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/usage"
)

// newBedrock reads AWS credentials the standard way (environment, shared
// config, instance or task role) for CloudWatch in one region.
func newBedrock(region string) (usage.Poller, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("agent: AWS config for Bedrock usage: %w", err)
	}
	return &usage.Bedrock{Client: cloudwatch.NewFromConfig(cfg), Region: region}, nil
}
