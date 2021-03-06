package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cortexproject/cortex/integration/e2e"
	e2edb "github.com/cortexproject/cortex/integration/e2e/db"
	"github.com/cortexproject/cortex/integration/e2ecortex"
)

func TestIngesterFlushWithChunksStorage(t *testing.T) {
	s, err := e2e.NewScenario()
	require.NoError(t, err)
	defer s.Close()

	// Start dependencies
	require.NoError(t, s.StartService(e2edb.NewDynamoDB()))
	require.NoError(t, s.StartService(e2edb.NewConsul()))
	require.NoError(t, s.WaitReady("consul", "dynamodb"))

	// Start Cortex components
	require.NoError(t, ioutil.WriteFile(
		filepath.Join(s.SharedDir(), cortexSchemaConfigFile),
		[]byte(cortexSchemaConfigYaml),
		os.ModePerm),
	)
	require.NoError(t, s.StartService(e2ecortex.NewTableManager("table-manager", ChunksStorage, "")))
	require.NoError(t, s.StartService(e2ecortex.NewIngester("ingester-1", mergeFlags(ChunksStorage, map[string]string{
		"-ingester.max-transfer-retries": "0",
	}), "")))
	require.NoError(t, s.StartService(e2ecortex.NewQuerier("querier", ChunksStorage, "")))
	require.NoError(t, s.StartService(e2ecortex.NewDistributor("distributor", ChunksStorage, "")))
	require.NoError(t, s.WaitReady("distributor", "querier", "ingester-1", "table-manager"))

	// Wait until the first table-manager sync has completed, so that we're
	// sure the tables have been created
	require.NoError(t, s.Service("table-manager").WaitMetric("cortex_dynamo_sync_tables_seconds", 1))

	// Wait until both the distributor and querier have updated the ring
	require.NoError(t, s.Service("distributor").WaitMetric("cortex_ring_tokens_total", 512))
	require.NoError(t, s.Service("querier").WaitMetric("cortex_ring_tokens_total", 512))

	c, err := e2ecortex.NewClient(s.Endpoint("distributor", 80), s.Endpoint("querier", 80), "user-1")
	require.NoError(t, err)

	// Push some series to Cortex
	now := time.Now()
	series1, expectedVector1 := generateSeries("series_1", now)
	series2, expectedVector2 := generateSeries("series_2", now)

	for _, series := range [][]prompb.TimeSeries{series1, series2} {
		res, err := c.Push(series)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
	}

	// Query the series
	result, err := c.Query("series_1", now)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())
	assert.Equal(t, expectedVector1, result.(model.Vector))

	result, err = c.Query("series_2", now)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())
	assert.Equal(t, expectedVector2, result.(model.Vector))

	// Stop ingester-1, so that it will flush all chunks to the storage. This function will return
	// once the ingester-1 is successfully stopped, which means the flushing is completed.
	require.NoError(t, s.StopService("ingester-1"))

	// Ensure chunks have been uploaded to the storage (DynamoDB).
	dynamoURL := "dynamodb://u:p@" + s.Service("dynamodb").Endpoint(8000)
	dynamoClient, err := newDynamoClient(dynamoURL)
	require.NoError(t, err)

	// We have pushed 2 series, so we do expect 2 chunks
	period := now.Unix() / (168 * 3600)
	indexTable := fmt.Sprintf("cortex_%d", period)
	chunksTable := fmt.Sprintf("cortex_chunks_%d", period)

	out, err := dynamoClient.Scan(&dynamodb.ScanInput{TableName: aws.String(indexTable)})
	require.NoError(t, err)
	assert.Equal(t, int64(2*2), *out.Count)

	out, err = dynamoClient.Scan(&dynamodb.ScanInput{TableName: aws.String(chunksTable)})
	require.NoError(t, err)
	assert.Equal(t, int64(2), *out.Count)
}
