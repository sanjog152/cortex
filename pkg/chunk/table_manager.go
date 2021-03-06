package chunk

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	"github.com/cortexproject/cortex/pkg/util"
	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/mtime"
)

const (
	readLabel  = "read"
	writeLabel = "write"
)

var (
	syncTableDuration = instrument.NewHistogramCollector(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "dynamo_sync_tables_seconds",
		Help:      "Time spent doing SyncTables.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"operation", "status_code"}))
	tableCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "dynamo_table_capacity_units",
		Help:      "Per-table DynamoDB capacity, measured in DynamoDB capacity units.",
	}, []string{"op", "table"})
)

func init() {
	prometheus.MustRegister(tableCapacity)
	syncTableDuration.Register()
}

// TableManagerConfig holds config for a TableManager
type TableManagerConfig struct {
	// Master 'off-switch' for table capacity updates, e.g. when troubleshooting
	ThroughputUpdatesDisabled bool

	// Master 'on-switch' for table retention deletions
	RetentionDeletesEnabled bool

	// How far back tables will be kept before they are deleted
	RetentionPeriod time.Duration

	// Period with which the table manager will poll for tables.
	DynamoDBPollInterval time.Duration

	// duration a table will be created before it is needed.
	CreationGracePeriod time.Duration

	IndexTables ProvisionConfig
	ChunkTables ProvisionConfig
}

// ProvisionConfig holds config for provisioning capacity (on DynamoDB)
type ProvisionConfig struct {
	ProvisionedThroughputOnDemandMode bool
	ProvisionedWriteThroughput        int64
	ProvisionedReadThroughput         int64
	InactiveThroughputOnDemandMode    bool
	InactiveWriteThroughput           int64
	InactiveReadThroughput            int64

	WriteScale              AutoScalingConfig
	InactiveWriteScale      AutoScalingConfig
	InactiveWriteScaleLastN int64
	ReadScale               AutoScalingConfig
	InactiveReadScale       AutoScalingConfig
	InactiveReadScaleLastN  int64
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *TableManagerConfig) RegisterFlags(f *flag.FlagSet) {
	f.BoolVar(&cfg.ThroughputUpdatesDisabled, "table-manager.throughput-updates-disabled", false, "If true, disable all changes to DB capacity")
	f.BoolVar(&cfg.RetentionDeletesEnabled, "table-manager.retention-deletes-enabled", false, "If true, enables retention deletes of DB tables")
	f.DurationVar(&cfg.RetentionPeriod, "table-manager.retention-period", 0, "Tables older than this retention period are deleted. Note: This setting is destructive to data!(default: 0, which disables deletion)")
	f.DurationVar(&cfg.DynamoDBPollInterval, "dynamodb.poll-interval", 2*time.Minute, "How frequently to poll DynamoDB to learn our capacity.")
	f.DurationVar(&cfg.CreationGracePeriod, "dynamodb.periodic-table.grace-period", 10*time.Minute, "DynamoDB periodic tables grace period (duration which table will be created/deleted before/after it's needed).")

	cfg.IndexTables.RegisterFlags("dynamodb.periodic-table", f)
	cfg.ChunkTables.RegisterFlags("dynamodb.chunk-table", f)
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *ProvisionConfig) RegisterFlags(argPrefix string, f *flag.FlagSet) {
	f.Int64Var(&cfg.ProvisionedWriteThroughput, argPrefix+".write-throughput", 3000, "DynamoDB table default write throughput.")
	f.Int64Var(&cfg.ProvisionedReadThroughput, argPrefix+".read-throughput", 300, "DynamoDB table default read throughput.")
	f.BoolVar(&cfg.ProvisionedThroughputOnDemandMode, argPrefix+".enable-ondemand-throughput-mode", false, "Enables on demand througput provisioning for the storage provider (if supported). Applies only to tables which are not autoscaled")
	f.Int64Var(&cfg.InactiveWriteThroughput, argPrefix+".inactive-write-throughput", 1, "DynamoDB table write throughput for inactive tables.")
	f.Int64Var(&cfg.InactiveReadThroughput, argPrefix+".inactive-read-throughput", 300, "DynamoDB table read throughput for inactive tables.")
	f.BoolVar(&cfg.InactiveThroughputOnDemandMode, argPrefix+".inactive-enable-ondemand-throughput-mode", false, "Enables on demand througput provisioning for the storage provider (if supported). Applies only to tables which are not autoscaled")

	cfg.WriteScale.RegisterFlags(argPrefix+".write-throughput.scale", f)
	cfg.InactiveWriteScale.RegisterFlags(argPrefix+".inactive-write-throughput.scale", f)
	f.Int64Var(&cfg.InactiveWriteScaleLastN, argPrefix+".inactive-write-throughput.scale-last-n", 4, "Number of last inactive tables to enable write autoscale.")

	cfg.ReadScale.RegisterFlags(argPrefix+".read-throughput.scale", f)
	cfg.InactiveReadScale.RegisterFlags(argPrefix+".inactive-read-throughput.scale", f)
	f.Int64Var(&cfg.InactiveReadScaleLastN, argPrefix+".inactive-read-throughput.scale-last-n", 4, "Number of last inactive tables to enable read autoscale.")
}

// Tags is a string-string map that implements flag.Value.
type Tags map[string]string

// String implements flag.Value
func (ts Tags) String() string {
	if ts == nil {
		return ""
	}

	return fmt.Sprintf("%v", map[string]string(ts))
}

// Set implements flag.Value
func (ts *Tags) Set(s string) error {
	if *ts == nil {
		*ts = map[string]string{}
	}

	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("tag must of the format key=value")
	}
	(*ts)[parts[0]] = parts[1]
	return nil
}

// Equals returns true is other matches ts.
func (ts Tags) Equals(other Tags) bool {
	if len(ts) != len(other) {
		return false
	}

	for k, v1 := range ts {
		v2, ok := other[k]
		if !ok || v1 != v2 {
			return false
		}
	}

	return true
}

// TableManager creates and manages the provisioned throughput on DynamoDB tables
type TableManager struct {
	client      TableClient
	cfg         TableManagerConfig
	schemaCfg   SchemaConfig
	maxChunkAge time.Duration
	done        chan struct{}
	wait        sync.WaitGroup
}

// NewTableManager makes a new TableManager
func NewTableManager(cfg TableManagerConfig, schemaCfg SchemaConfig, maxChunkAge time.Duration, tableClient TableClient) (*TableManager, error) {
	return &TableManager{
		cfg:         cfg,
		schemaCfg:   schemaCfg,
		maxChunkAge: maxChunkAge,
		client:      tableClient,
		done:        make(chan struct{}),
	}, nil
}

// Start the TableManager
func (m *TableManager) Start() {
	m.wait.Add(1)
	go m.loop()
}

// Stop the TableManager
func (m *TableManager) Stop() {
	close(m.done)
	m.wait.Wait()
}

func (m *TableManager) loop() {
	defer m.wait.Done()

	ticker := time.NewTicker(m.cfg.DynamoDBPollInterval)
	defer ticker.Stop()

	if err := instrument.CollectedRequest(context.Background(), "TableManager.SyncTables", syncTableDuration, instrument.ErrorCode, func(ctx context.Context) error {
		return m.SyncTables(ctx)
	}); err != nil {
		level.Error(util.Logger).Log("msg", "error syncing tables", "err", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := instrument.CollectedRequest(context.Background(), "TableManager.SyncTables", syncTableDuration, instrument.ErrorCode, func(ctx context.Context) error {
				return m.SyncTables(ctx)
			}); err != nil {
				level.Error(util.Logger).Log("msg", "error syncing tables", "err", err)
			}
		case <-m.done:
			return
		}
	}
}

// SyncTables will calculate the tables expected to exist, create those that do
// not and update those that need it.  It is exposed for testing.
func (m *TableManager) SyncTables(ctx context.Context) error {
	expected := m.calculateExpectedTables()
	level.Info(util.Logger).Log("msg", "synching tables", "num_expected_tables", len(expected), "expected_tables", len(expected))

	toCreate, toCheckThroughput, toDelete, err := m.partitionTables(ctx, expected)
	if err != nil {
		return err
	}

	if err := m.deleteTables(ctx, toDelete); err != nil {
		return err
	}

	if err := m.createTables(ctx, toCreate); err != nil {
		return err
	}

	return m.updateTables(ctx, toCheckThroughput)
}

func (m *TableManager) calculateExpectedTables() []TableDesc {
	result := []TableDesc{}

	for i, config := range m.schemaCfg.Configs {
		if config.From.Time().After(mtime.Now()) {
			continue
		}
		if config.IndexTables.Period == 0 { // non-periodic table
			if len(result) > 0 && result[len(result)-1].Name == config.IndexTables.Prefix {
				continue // already got a non-periodic table with this name
			}

			table := TableDesc{
				Name:              config.IndexTables.Prefix,
				ProvisionedRead:   m.cfg.IndexTables.InactiveReadThroughput,
				ProvisionedWrite:  m.cfg.IndexTables.InactiveWriteThroughput,
				UseOnDemandIOMode: m.cfg.IndexTables.InactiveThroughputOnDemandMode,
				Tags:              config.IndexTables.Tags,
			}
			isActive := true
			if i+1 < len(m.schemaCfg.Configs) {
				var (
					endTime         = m.schemaCfg.Configs[i+1].From.Unix()
					gracePeriodSecs = int64(m.cfg.CreationGracePeriod / time.Second)
					maxChunkAgeSecs = int64(m.maxChunkAge / time.Second)
					now             = mtime.Now().Unix()
				)
				if now >= endTime+gracePeriodSecs+maxChunkAgeSecs {
					isActive = false

				}
			}
			if isActive {
				table.ProvisionedRead = m.cfg.IndexTables.ProvisionedReadThroughput
				table.ProvisionedWrite = m.cfg.IndexTables.ProvisionedWriteThroughput
				table.UseOnDemandIOMode = m.cfg.IndexTables.ProvisionedThroughputOnDemandMode
				if m.cfg.IndexTables.WriteScale.Enabled {
					table.WriteScale = m.cfg.IndexTables.WriteScale
					table.UseOnDemandIOMode = false
				}
				if m.cfg.IndexTables.ReadScale.Enabled {
					table.ReadScale = m.cfg.IndexTables.ReadScale
					table.UseOnDemandIOMode = false
				}
			}
			result = append(result, table)
		} else {
			endTime := mtime.Now().Add(m.cfg.CreationGracePeriod)
			if i+1 < len(m.schemaCfg.Configs) {
				nextFrom := m.schemaCfg.Configs[i+1].From.Time()
				if endTime.After(nextFrom) {
					endTime = nextFrom
				}
			}
			endModelTime := model.TimeFromUnix(endTime.Unix())
			result = append(result, config.IndexTables.periodicTables(
				config.From, endModelTime, m.cfg.IndexTables, m.cfg.CreationGracePeriod, m.maxChunkAge, m.cfg.RetentionPeriod,
			)...)
			if config.ChunkTables.Prefix != "" {
				result = append(result, config.ChunkTables.periodicTables(
					config.From, endModelTime, m.cfg.ChunkTables, m.cfg.CreationGracePeriod, m.maxChunkAge, m.cfg.RetentionPeriod,
				)...)
			}
		}
	}

	sort.Sort(byName(result))
	return result
}

// partitionTables works out tables that need to be created vs tables that need to be updated
func (m *TableManager) partitionTables(ctx context.Context, descriptions []TableDesc) ([]TableDesc, []TableDesc, []TableDesc, error) {
	existingTables, err := m.client.ListTables(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Strings(existingTables)

	tablePrefixes := map[string]struct{}{}
	for _, cfg := range m.schemaCfg.Configs {
		tablePrefixes[cfg.IndexTables.Prefix] = struct{}{}
		tablePrefixes[cfg.ChunkTables.Prefix] = struct{}{}
	}

	toCreate, toCheck, toDelete := []TableDesc{}, []TableDesc{}, []TableDesc{}
	i, j := 0, 0
	for i < len(descriptions) && j < len(existingTables) {
		if descriptions[i].Name < existingTables[j] {
			// Table descriptions[i] doesn't exist
			toCreate = append(toCreate, descriptions[i])
			i++
		} else if descriptions[i].Name > existingTables[j] {
			// existingTables[j].name isn't in descriptions, and can be removed
			if m.cfg.RetentionPeriod > 0 {
				for tblPrefix := range tablePrefixes {
					if strings.HasPrefix(existingTables[j], tblPrefix) {
						toDelete = append(toDelete, TableDesc{Name: existingTables[j]})
						break
					}
				}
			}
			j++
		} else {
			// Table exists, need to check it has correct throughput
			toCheck = append(toCheck, descriptions[i])
			i++
			j++
		}
	}
	for ; i < len(descriptions); i++ {
		toCreate = append(toCreate, descriptions[i])
	}

	return toCreate, toCheck, toDelete, nil
}

func (m *TableManager) createTables(ctx context.Context, descriptions []TableDesc) error {
	for _, desc := range descriptions {
		level.Info(util.Logger).Log("msg", "creating table", "table", desc.Name)
		err := m.client.CreateTable(ctx, desc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *TableManager) deleteTables(ctx context.Context, descriptions []TableDesc) error {
	for _, desc := range descriptions {
		level.Info(util.Logger).Log("msg", "table has exceeded the retention period", "table", desc.Name)
		if !m.cfg.RetentionDeletesEnabled {
			continue
		}

		level.Info(util.Logger).Log("msg", "deleting table", "table", desc.Name)
		err := m.client.DeleteTable(ctx, desc.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *TableManager) updateTables(ctx context.Context, descriptions []TableDesc) error {
	for _, expected := range descriptions {
		level.Debug(util.Logger).Log("msg", "checking provisioned throughput on table", "table", expected.Name)
		current, isActive, err := m.client.DescribeTable(ctx, expected.Name)
		if err != nil {
			return err
		}

		tableCapacity.WithLabelValues(readLabel, expected.Name).Set(float64(current.ProvisionedRead))
		tableCapacity.WithLabelValues(writeLabel, expected.Name).Set(float64(current.ProvisionedWrite))

		if m.cfg.ThroughputUpdatesDisabled {
			continue
		}

		if !isActive {
			level.Info(util.Logger).Log("msg", "skipping update on table, not yet ACTIVE", "table", expected.Name)
			continue
		}

		if expected.Equals(current) {
			level.Info(util.Logger).Log("msg", "provisioned throughput on table, skipping", "table", current.Name, "read", current.ProvisionedRead, "write", current.ProvisionedWrite)
			continue
		}

		err = m.client.UpdateTable(ctx, current, expected)
		if err != nil {
			return err
		}
	}
	return nil
}

// ExpectTables compares existing tables to an expected set of tables.  Exposed
// for testing,
func ExpectTables(ctx context.Context, client TableClient, expected []TableDesc) error {
	tables, err := client.ListTables(ctx)
	if err != nil {
		return err
	}

	if len(expected) != len(tables) {
		return fmt.Errorf("Unexpected number of tables: %v != %v", expected, tables)
	}

	sort.Strings(tables)
	sort.Sort(byName(expected))

	for i, expect := range expected {
		if tables[i] != expect.Name {
			return fmt.Errorf("Expected '%s', found '%s'", expect.Name, tables[i])
		}

		desc, _, err := client.DescribeTable(ctx, expect.Name)
		if err != nil {
			return err
		}

		if !desc.Equals(expect) {
			return fmt.Errorf("Expected '%#v', found '%#v' for table '%s'", expect, desc, desc.Name)
		}
	}

	return nil
}
