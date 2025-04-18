package graphite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/httpclient"
	"github.com/grafana/grafana/pkg/infra/tracing"
)

func TestFixIntervalFormat(t *testing.T) {
	testCases := []struct {
		name     string
		target   string
		expected string
	}{
		{
			name:     "should transform 1m to graphite unit (1min) when used as interval string",
			target:   "aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1m'), 4)",
			expected: "aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1min'), 4)",
		},
		{
			name:     "should transform 1M to graphite unit (1mon) when used as interval string",
			target:   "aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1M'), 4)",
			expected: "aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1mon'), 4)",
		},
		{
			name:     "should not transform 1m when not used as interval string",
			target:   "app.grafana.*.dashboards.views.1m.count",
			expected: "app.grafana.*.dashboards.views.1m.count",
		},
		{
			name:     "should not transform 1M when not used as interval string",
			target:   "app.grafana.*.dashboards.views.1M.count",
			expected: "app.grafana.*.dashboards.views.1M.count",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tr := fixIntervalFormat(tc.target)
			assert.Equal(t, tc.expected, tr)
		})
	}
}

func TestProcessQueries(t *testing.T) {
	service := &Service{}
	log := logger.FromContext(context.Background())
	t.Run("Parses single valid query", func(t *testing.T) {
		queries := []backend.DataQuery{
			{
				RefID: "A",
				JSON: []byte(`{
					"target": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
		}
		targets, invalids, mapping, err := service.processQueries(log, queries)
		assert.NoError(t, err)
		assert.Empty(t, invalids)
		assert.Len(t, mapping, 1)
		assert.Len(t, targets, 1)
		assert.Equal(t, "aliasSub(app.grafana.*.dashboards.views.1M.count,\"(^.*$)\",\"\\1 A\")", targets[0])
	})

	t.Run("Parses multiple valid queries with refId mappings", func(t *testing.T) {
		queries := []backend.DataQuery{
			{
				RefID: "A",
				JSON: []byte(`{
					"target": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
			{
				RefID: "query B",
				JSON: []byte(`{
					"target": "aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1mon'), 4)"
				}`),
			},
		}
		targets, invalids, mapping, err := service.processQueries(log, queries)
		assert.NoError(t, err)
		assert.Empty(t, invalids)
		assert.Len(t, mapping, 2)
		assert.Len(t, targets, 2)
		assert.Equal(t, "aliasSub(app.grafana.*.dashboards.views.1M.count,\"(^.*$)\",\"\\1 A\")", targets[0])
		assert.Equal(t, "aliasSub(aliasByNode(hitcount(averageSeries(app.grafana.*.dashboards.views.count), '1mon'), 4),\"(^.*$)\",\"\\1 query_B\")", targets[1])
	})

	t.Run("Parses multiple queries with one invalid", func(t *testing.T) {
		queries := []backend.DataQuery{
			{
				RefID: "A",
				JSON: []byte(`{
					"target": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
			{
				RefID: "B",
				JSON: []byte(`{
					"query": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
		}
		targets, invalids, mapping, err := service.processQueries(log, queries)
		assert.NoError(t, err)
		assert.Len(t, invalids, 1)
		assert.Len(t, mapping, 1)
		assert.Len(t, targets, 1)
		json, _ := simplejson.NewJson(queries[1].JSON)
		expectedInvalid := fmt.Sprintf("Query: %v has no target", json)
		assert.Equal(t, expectedInvalid, invalids[0])
	})

	t.Run("QueryData with no valid queries returns bad request response", func(t *testing.T) {
		queries := []backend.DataQuery{
			{
				RefID: "A",
				JSON: []byte(`{
					"query": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
			{
				RefID: "B",
				JSON: []byte(`{
					"query": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
		}

		service := ProvideService(httpclient.NewProvider(), tracing.NewNoopTracerService())

		rsp, err := service.QueryData(context.Background(), &backend.QueryDataRequest{
			PluginContext: backend.PluginContext{
				DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
					ID:  0,
					URL: "http://localhost",
				},
			},
			Queries: queries,
		})
		assert.NoError(t, err)
		expectedResponse := backend.ErrDataResponseWithSource(400, backend.ErrorSourceDownstream, "no query target found for the alert rule")
		assert.Equal(t, expectedResponse, rsp.Responses["A"])
	})

	t.Run("QueryData with no queries returns an error", func(t *testing.T) {
		service := &Service{}

		rsp, err := service.QueryData(context.Background(), &backend.QueryDataRequest{})
		assert.Nil(t, rsp)
		assert.Error(t, err)
	})

	t.Run("QueryData happy path with service provider and plugin context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{
					"target": "target A",
					"tags": { "fooTag": "fooValue", "barTag": "barValue", "int": 100, "float": 3.14 },
					"datapoints": [[50, 1], [null, 2], [100, 3]]
				}	
			]`))
		}))
		t.Cleanup(server.Close)

		service := ProvideService(httpclient.NewProvider(), tracing.NewNoopTracerService())

		queries := []backend.DataQuery{
			{
				RefID: "A",
				JSON: []byte(`{
					"target": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
			{
				RefID: "B",
				JSON: []byte(`{
					"query": "app.grafana.*.dashboards.views.1M.count"
				}`),
			},
		}

		rsp, err := service.QueryData(context.Background(), &backend.QueryDataRequest{
			PluginContext: backend.PluginContext{
				DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
					ID:  0,
					URL: server.URL,
				},
			},
			Queries: queries,
		})
		assert.NoError(t, err)
		assert.NotNil(t, rsp)
	})
}

func TestConvertResponses(t *testing.T) {
	service := &Service{}

	t.Run("Converts response without tags to data frames", func(*testing.T) {
		body := `
		[
			{
				"target": "target A",
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			}
		]`
		a := 50.0
		b := 100.0
		expectedFrame := data.NewFrame("A",
			data.NewField("time", nil, []time.Time{time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()}),
			data.NewField("value", data.Labels{}, []*float64{&a, nil, &b}).SetConfig(&data.FieldConfig{DisplayNameFromDS: "target"}),
		).SetMeta(&data.FrameMeta{Type: data.FrameTypeTimeSeriesMulti})
		expectedFrames := data.Frames{expectedFrame}

		httpResponse := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
		dataFrames, err := service.toDataFrames(logger, httpResponse, map[string]string{})

		require.NoError(t, err)
		if !reflect.DeepEqual(expectedFrames, dataFrames) {
			expectedFramesJSON, _ := json.Marshal(expectedFrames)
			dataFramesJSON, _ := json.Marshal(dataFrames)
			t.Errorf("Data frames should have been equal but was, expected:\n%s\nactual:\n%s", expectedFramesJSON, dataFramesJSON)
		}
	})

	t.Run("Converts response with tags to data frames", func(*testing.T) {
		body := `
		[
			{
				"target": "target A",
				"tags": { "fooTag": "fooValue", "barTag": "barValue", "int": 100, "float": 3.14 },
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			}
		]`
		a := 50.0
		b := 100.0
		expectedFrame := data.NewFrame("A",
			data.NewField("time", nil, []time.Time{time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()}),
			data.NewField("value", data.Labels{
				"fooTag": "fooValue",
				"barTag": "barValue",
				"int":    "100",
				"float":  "3.14",
			}, []*float64{&a, nil, &b}).SetConfig(&data.FieldConfig{DisplayNameFromDS: "target"}),
		).SetMeta(&data.FrameMeta{Type: data.FrameTypeTimeSeriesMulti})
		expectedFrames := data.Frames{expectedFrame}

		httpResponse := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
		dataFrames, err := service.toDataFrames(logger, httpResponse, map[string]string{})

		require.NoError(t, err)
		if !reflect.DeepEqual(expectedFrames, dataFrames) {
			expectedFramesJSON, _ := json.Marshal(expectedFrames)
			dataFramesJSON, _ := json.Marshal(dataFrames)
			t.Errorf("Data frames should have been equal but was, expected:\n%s\nactual:\n%s", expectedFramesJSON, dataFramesJSON)
		}
	})

	t.Run("Converts response with multiple targets", func(*testing.T) {
		body := `
		[
			{
				"target": "target 1 A",
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			},
			{
				"target": "target 2 B",
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			}
		]`
		a := 50.0
		b := 100.0
		expectedFrameA := data.NewFrame("A",
			data.NewField("time", nil, []time.Time{time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()}),
			data.NewField("value", data.Labels{}, []*float64{&a, nil, &b}).SetConfig(&data.FieldConfig{DisplayNameFromDS: "target 1"}),
		).SetMeta(&data.FrameMeta{Type: data.FrameTypeTimeSeriesMulti})
		expectedFrameB := data.NewFrame("B",
			data.NewField("time", nil, []time.Time{time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()}),
			data.NewField("value", data.Labels{}, []*float64{&a, nil, &b}).SetConfig(&data.FieldConfig{DisplayNameFromDS: "target 2"}),
		).SetMeta(&data.FrameMeta{Type: data.FrameTypeTimeSeriesMulti})
		expectedFrames := data.Frames{expectedFrameA, expectedFrameB}

		httpResponse := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
		dataFrames, err := service.toDataFrames(logger, httpResponse, map[string]string{})

		require.NoError(t, err)
		if !reflect.DeepEqual(expectedFrames, dataFrames) {
			expectedFramesJSON, _ := json.Marshal(expectedFrames)
			dataFramesJSON, _ := json.Marshal(dataFrames)
			t.Errorf("Data frames should have been equal but was, expected:\n%s\nactual:\n%s", expectedFramesJSON, dataFramesJSON)
		}
	})

	t.Run("Converts response with refId mapping", func(*testing.T) {
		body := `
		[
			{
				"target": "target A_A",
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			}
		]`
		a := 50.0
		b := 100.0
		expectedFrame := data.NewFrame("A A",
			data.NewField("time", nil, []time.Time{time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()}),
			data.NewField("value", data.Labels{}, []*float64{&a, nil, &b}).SetConfig(&data.FieldConfig{DisplayNameFromDS: "target"}),
		).SetMeta(&data.FrameMeta{Type: data.FrameTypeTimeSeriesMulti})
		expectedFrames := data.Frames{expectedFrame}

		httpResponse := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
		dataFrames, err := service.toDataFrames(logger, httpResponse, map[string]string{"A_A": "A A"})

		require.NoError(t, err)
		if !reflect.DeepEqual(expectedFrames, dataFrames) {
			expectedFramesJSON, _ := json.Marshal(expectedFrames)
			dataFramesJSON, _ := json.Marshal(dataFrames)
			t.Errorf("Data frames should have been equal but was, expected:\n%s\nactual:\n%s", expectedFramesJSON, dataFramesJSON)
		}
	})

	t.Run("Chokes on response with invalid target name", func(*testing.T) {
		body := `
		[
			{
				"target": "target",
				"datapoints": [[50, 1], [null, 2], [100, 3]]
			}
		]`
		httpResponse := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
		_, err := service.toDataFrames(logger, httpResponse, map[string]string{})
		require.Error(t, err)
	})
}
