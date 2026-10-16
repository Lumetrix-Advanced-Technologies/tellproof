//go:build bedrock

package usage

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// fakeCloudWatch answers like CloudWatch for two models over two hours, with
// paged ListMetrics and GetMetricData.
type fakeCloudWatch struct {
	queries []cwtypes.MetricDataQuery
	calls   int
}

func (f *fakeCloudWatch) ListMetrics(_ context.Context, in *cloudwatch.ListMetricsInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	dim := func(m string) cwtypes.Metric {
		return cwtypes.Metric{Namespace: aws.String("AWS/Bedrock"), MetricName: aws.String("Invocations"),
			Dimensions: []cwtypes.Dimension{{Name: aws.String("ModelId"), Value: aws.String(m)}}}
	}
	if in.NextToken == nil {
		return &cloudwatch.ListMetricsOutput{Metrics: []cwtypes.Metric{dim("anthropic.claude-sonnet-4-5-20250929-v1:0")}, NextToken: aws.String("n2")}, nil
	}
	// A metric with an extra dimension is a different series; it is skipped.
	extra := dim("amazon.titan-embed-text-v2:0")
	extra.Dimensions = append(extra.Dimensions, cwtypes.Dimension{Name: aws.String("Other"), Value: aws.String("x")})
	return &cloudwatch.ListMetricsOutput{Metrics: []cwtypes.Metric{dim("amazon.titan-embed-text-v2:0"), extra}}, nil
}

func (f *fakeCloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.calls++
	f.queries = in.MetricDataQueries
	ts := []time.Time{h10, h11}
	vals := map[string][]float64{ // model index_metric index -> per-hour sums
		"m0_0": {120000, 70000}, "m0_1": {40000, 20000}, "m0_2": {300, 150},
		"m1_0": {9000, 1000}, "m1_1": {0, 0}, "m1_2": {90, 10},
	}
	var out []cwtypes.MetricDataResult
	for _, id := range []string{"m0_0", "m0_1", "m0_2", "m1_0", "m1_1", "m1_2"} { // fixed order: pages must not overlap
		out = append(out, cwtypes.MetricDataResult{Id: aws.String(id), Timestamps: ts, Values: vals[id]})
	}
	if in.NextToken == nil { // first page: half the results
		return &cloudwatch.GetMetricDataOutput{MetricDataResults: out[:3], NextToken: aws.String("p2")}, nil
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: out[3:]}, nil
}

func TestBedrockPoller(t *testing.T) {
	cw := &fakeCloudWatch{}
	got, err := (&Bedrock{Client: cw, Region: "ap-southeast-2"}).Poll(context.Background(), h10, h10.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	u, ok := find(got, h10, "aws.bedrock", "anthropic.claude-sonnet-4-5-20250929-v1:0")
	if !ok || u.InputTokens != 120000 || u.OutputTokens != 40000 || *u.Requests != 300 || u.Region != "ap-southeast-2" {
		t.Errorf("sonnet at 10:00 = %+v", u)
	}
	if u, ok := find(got, h11, "aws.bedrock", "amazon.titan-embed-text-v2:0"); !ok || u.InputTokens != 1000 || *u.Requests != 10 {
		t.Errorf("titan at 11:00 = %+v", u)
	}
	if len(cw.queries) != 6 || cw.calls != 2 {
		t.Errorf("%d queries over %d calls; want 6 over 2 pages", len(cw.queries), cw.calls)
	}
	for _, q := range cw.queries {
		if aws.ToInt32(q.MetricStat.Period) != 3600 || aws.ToString(q.MetricStat.Stat) != "Sum" {
			t.Errorf("query %s: period %d stat %s", aws.ToString(q.Id), aws.ToInt32(q.MetricStat.Period), aws.ToString(q.MetricStat.Stat))
		}
	}
}
