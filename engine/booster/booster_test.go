package booster

import (
	"testing"

	"github.com/Parsaetak/FreeIran/engine/mempressure"
)

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	if l.QueueConcurrencyMax <= l.QueueConcurrencyMin {
		t.Error("QueueConcurrency range inverted")
	}
	if l.CacheEntriesMax <= l.CacheEntriesMin {
		t.Error("CacheEntries range inverted")
	}
}

func TestNewClampsToLimits(t *testing.T) {
	c := New(Limits{
		QueueConcurrencyMin:     2,
		QueueConcurrencyMax:     4,
		IngestionConcurrencyMin: 1,
		IngestionConcurrencyMax: 3,
		ParserConcurrencyMin:    1,
		ParserConcurrencyMax:    2,
		BatchSizeMin:            100,
		BatchSizeMax:            200,
		QueueDepthMin:           500,
		QueueDepthMax:           1000,
		CacheEntriesMin:         100,
		CacheEntriesMax:         500,
		ChunkFlushBytesMin:      1000,
		ChunkFlushBytesMax:      2000,
	}, nil)

	s := c.Settings()
	if s.QueueConcurrency < 2 || s.QueueConcurrency > 4 {
		t.Errorf("QueueConcurrency = %d, want in [2,4]", s.QueueConcurrency)
	}
	if s.BatchSize < 100 || s.BatchSize > 200 {
		t.Errorf("BatchSize = %d, want in [100,200]", s.BatchSize)
	}
}

func TestTickCriticalHitsFloor(t *testing.T) {
	// Build a mempressure controller we can drive to Critical.
	mc := mempressure.New(mempressure.Ceiling{
		HeapBytes:  1 << 40,
		RSSBytes:   1 << 40,
		GCPct:      100,
		ArenaBytes: 1 << 40,
		CacheBytes: 1000,
		QueueBytes: 1 << 40,
	})
	mc.SetCacheBytes(990) // 99% → Critical
	mc.Sample()           // update the state

	c := New(DefaultLimits(), mc)
	c.Tick()
	s := c.Settings()
	l := DefaultLimits()
	if s.QueueConcurrency != l.QueueConcurrencyMin {
		t.Errorf("Critical: QueueConcurrency = %d, want %d (floor)", s.QueueConcurrency, l.QueueConcurrencyMin)
	}
	if s.CacheEntries != l.CacheEntriesMin {
		t.Errorf("Critical: CacheEntries = %d, want %d (floor)", s.CacheEntries, l.CacheEntriesMin)
	}
	if s.BatchSize != l.BatchSizeMin {
		t.Errorf("Critical: BatchSize = %d, want %d (floor)", s.BatchSize, l.BatchSizeMin)
	}
}

func TestTickNormalGrowsOnBacklog(t *testing.T) {
	mc := mempressure.New(mempressure.DefaultCeiling())

	c := New(DefaultLimits(), mc)
	initial := c.Settings()

	// Report deep backlog and low CPU.
	c.Inputs().QueueBacklog.Store(10000)
	c.Inputs().SetCPUPressure(0.3)

	// Tick several times; concurrency should grow toward max.
	for i := 0; i < 20; i++ {
		c.Tick()
	}
	s := c.Settings()
	if s.QueueConcurrency <= initial.QueueConcurrency {
		t.Errorf("after 20 ticks with backlog, QueueConcurrency = %d, want > %d", s.QueueConcurrency, initial.QueueConcurrency)
	}
}

func TestTickRespectsCeiling(t *testing.T) {
	mc := mempressure.New(mempressure.DefaultCeiling())
	c := New(DefaultLimits(), mc)
	c.Inputs().QueueBacklog.Store(1000000)
	c.Inputs().SetCPUPressure(0.1)

	for i := 0; i < 100; i++ {
		c.Tick()
	}
	s := c.Settings()
	l := DefaultLimits()
	if s.QueueConcurrency > l.QueueConcurrencyMax {
		t.Errorf("QueueConcurrency = %d, exceeds max %d", s.QueueConcurrency, l.QueueConcurrencyMax)
	}
	if s.IngestionConcurrency > l.IngestionConcurrencyMax {
		t.Errorf("IngestionConcurrency = %d, exceeds max %d", s.IngestionConcurrency, l.IngestionConcurrencyMax)
	}
}

func TestOnChangeFires(t *testing.T) {
	mc := mempressure.New(mempressure.Ceiling{
		HeapBytes:  1 << 40,
		RSSBytes:   1 << 40,
		GCPct:      100,
		ArenaBytes: 1 << 40,
		CacheBytes: 1000,
		QueueBytes: 1 << 40,
	})

	c := New(DefaultLimits(), mc)

	changes := 0
	c.OnChange(func(s Settings) {
		changes++
	})

	mc.SetCacheBytes(990) // → Critical
	mc.Sample()
	c.Tick()
	if changes == 0 {
		t.Error("OnChange did not fire on Critical transition")
	}
}

func TestInputsAccessors(t *testing.T) {
	var i Inputs
	i.SetCacheHitRate(0.75)
	if got := i.CacheHitRateF(); got < 0.74 || got > 0.76 {
		t.Errorf("CacheHitRateF = %f, want ~0.75", got)
	}
	i.SetThroughput(123.4)
	if got := i.ThroughputF(); got < 123 || got > 124 {
		t.Errorf("ThroughputF = %f, want ~123.4", got)
	}
	i.SetCPUPressure(0.5)
	if got := i.CPUPressureF(); got < 0.49 || got > 0.51 {
		t.Errorf("CPUPressureF = %f, want ~0.5", got)
	}
}
