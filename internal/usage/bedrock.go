//go:build bedrock

package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
)

// CloudWatchAPI is the part of the CloudWatch client the Bedrock poller uses.
type CloudWatchAPI interface {
	ListMetrics(ctx context.Context, in *cloudwatch.ListMetricsInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Bedrock reads Amazon Bedrock runtime metrics (namespace AWS/Bedrock,
// dimension ModelId: InputTokenCount, OutputTokenCount, Invocations) from
// CloudWatch in one region, in one-hour periods. Built only with -tags
// bedrock, so default agent builds stay standard-library only (D-010).
type Bedrock struct {
	Client CloudWatchAPI
	Region string
}

var bedrockMetrics = [3]string{"InputTokenCount", "OutputTokenCount", "Invocations"}

// Poll implements Poller.
func (b *Bedrock) Poll(ctx context.Context, start, end time.Time) ([]HourUsage, error) {
	if err := checkWindow(start, end, "cloudwatch"); err != nil {
		return nil, err
	}
	models, err := b.modelIDs(ctx)
	if err != nil {
		return nil, err
	}
	rows := map[time.Time][]batch.Usage{}
	const perCall = 150 // 3 queries per model; GetMetricData takes at most 500 queries
	for from := 0; from < len(models); from += perCall {
		chunk := models[from:min(from+perCall, len(models))]
		var queries []cwtypes.MetricDataQuery
		for i, model := range chunk {
			for j, metric := range bedrockMetrics {
				queries = append(queries, cwtypes.MetricDataQuery{
					Id: aws.String(fmt.Sprintf("m%d_%d", i, j)),
					MetricStat: &cwtypes.MetricStat{
						Metric: &cwtypes.Metric{Namespace: aws.String("AWS/Bedrock"), MetricName: aws.String(metric),
							Dimensions: []cwtypes.Dimension{{Name: aws.String("ModelId"), Value: aws.String(model)}}},
						Period: aws.Int32(3600),
						Stat:   aws.String("Sum"),
					},
				})
			}
		}
		// values[model][hour] = [input, output, invocations]
		values := map[string]map[time.Time]*[3]float64{}
		var token *string
		for n := 0; ; n++ {
			if n == maxPages {
				return nil, fmt.Errorf("usage: bedrock: more than %d pages", maxPages)
			}
			out, err := b.Client.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{MetricDataQueries: queries,
				StartTime: aws.Time(start), EndTime: aws.Time(end), ScanBy: cwtypes.ScanByTimestampAscending, NextToken: token})
			if err != nil {
				return nil, fmt.Errorf("usage: bedrock GetMetricData: %w", err)
			}
			for _, r := range out.MetricDataResults {
				var i, j int
				if _, err := fmt.Sscanf(aws.ToString(r.Id), "m%d_%d", &i, &j); err != nil || i >= len(chunk) || j >= 3 {
					continue
				}
				model := chunk[i]
				for k, ts := range r.Timestamps {
					if k >= len(r.Values) {
						break
					}
					h := ts.UTC().Truncate(time.Hour)
					if values[model] == nil {
						values[model] = map[time.Time]*[3]float64{}
					}
					if values[model][h] == nil {
						values[model][h] = &[3]float64{}
					}
					values[model][h][j] += r.Values[k]
				}
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			token = out.NextToken
		}
		for model, hours := range values {
			for h, v := range hours {
				reqs := int64(v[2])
				rows[h] = append(rows[h], batch.Usage{Provider: "aws.bedrock", Model: model, Region: b.Region,
					InputTokens: int64(v[0]), OutputTokens: int64(v[1]), Requests: &reqs})
			}
		}
	}
	return collect(rows), nil
}

// modelIDs lists the ModelId values that have Bedrock invocation metrics.
// Requests that fail before a model is resolved carry no ModelId and are not
// counted (docs/library-docs.md).
func (b *Bedrock) modelIDs(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	var token *string
	for n := 0; ; n++ {
		if n == maxPages {
			return nil, fmt.Errorf("usage: bedrock: more than %d metric pages", maxPages)
		}
		out, err := b.Client.ListMetrics(ctx, &cloudwatch.ListMetricsInput{Namespace: aws.String("AWS/Bedrock"),
			MetricName: aws.String("Invocations"), NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("usage: bedrock ListMetrics: %w", err)
		}
		for _, m := range out.Metrics {
			for _, d := range m.Dimensions {
				if aws.ToString(d.Name) == "ModelId" && len(m.Dimensions) == 1 && !seen[aws.ToString(d.Value)] {
					seen[aws.ToString(d.Value)] = true
					ids = append(ids, aws.ToString(d.Value))
				}
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return ids, nil
		}
		token = out.NextToken
	}
}
