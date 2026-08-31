// Copyright 2026 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/palantir/policy-bot/server/githubclient"
	"github.com/prometheus/client_golang/prometheus"
	gometrics "github.com/rcrowley/go-metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricIntegerConversions(t *testing.T) {
	assert.Equal(t, int64(0), saturatingUint64ToInt64(0))
	assert.Equal(t, int64(math.MaxInt64), saturatingUint64ToInt64(math.MaxUint64))
	assert.Equal(t, uint64(0), nonNegativeInt64ToUint64(-1))
	assert.Equal(t, uint64(math.MaxInt64), nonNegativeInt64ToUint64(math.MaxInt64))
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGitHubTransportRecordsRateLimitByInstallation(t *testing.T) {
	metrics, err := NewMetrics()
	require.NoError(t, err)

	transport := metrics.WrapTransport(testRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header: http.Header{
				"X-Ratelimit-Limit":     []string{"5000"},
				"X-Ratelimit-Remaining": []string{"4997"},
				"X-Ratelimit-Used":      []string{"3"},
			},
		}, nil
	}))

	ctx := githubclient.ContextWithInstallationID(context.Background(), 42)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos", nil)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	got, ok := metrics.rateLimits.Load(int64(42))
	require.True(t, ok)
	assert.Equal(t, &rateLimitState{limit: 5000, remaining: 4997, used: 3}, got)
	_, storedAsZero := metrics.rateLimits.Load(int64(0))
	assert.False(t, storedAsZero)
}

func TestGoMetricsCollectorExportsDistributionMetricsAsSummaries(t *testing.T) {
	registry := gometrics.NewRegistry()
	histogram := gometrics.NewHistogram(gometrics.NewUniformSample(10))
	histogram.Update(10)
	histogram.Update(20)
	require.NoError(t, registry.Register("github.event.age", histogram))

	timer := gometrics.NewTimer()
	timer.Update(10 * time.Millisecond)
	timer.Update(20 * time.Millisecond)
	require.NoError(t, registry.Register("github.request.duration", timer))

	promRegistry := prometheus.NewPedanticRegistry()
	promRegistry.MustRegister(NewGoMetricsCollector(registry, ""))
	families, err := promRegistry.Gather()
	require.NoError(t, err)

	found := make(map[string]bool)
	for _, family := range families {
		if family.GetName() != "github_event_age" && family.GetName() != "github_request_duration" {
			continue
		}
		found[family.GetName()] = true
		assert.Equal(t, "SUMMARY", family.GetType().String())
		require.Len(t, family.GetMetric(), 1)
		summary := family.GetMetric()[0].GetSummary()
		require.NotNil(t, summary)
		assert.Equal(t, uint64(2), summary.GetSampleCount())
		assert.Len(t, summary.GetQuantile(), 3)
	}

	assert.True(t, found["github_event_age"])
	assert.True(t, found["github_request_duration"])
}
