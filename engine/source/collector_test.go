package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Parsaetak/FreeIran/engine/config"
)

func TestCollect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/plain")

		_, _ = w.Write([]byte(
			"vless://11111111-1111-1111-1111-111111111111@example.com:443",
		))
	}))
	defer server.Close()

	collector := NewCollector()

	result, err := collector.Collect(
		context.Background(),
		Source{
			ID:      "test",
			Name:    "Test",
			URL:     server.URL,
			Enabled: true,
		},
	)

	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}

	if len(result.Configurations) != 1 {
		t.Fatalf(
			"expected 1 configuration, got %d",
			len(result.Configurations),
		)
	}

	if result.Source.ID != "test" {
		t.Fatalf(
			"expected source ID %q, got %q",
			"test",
			result.Source.ID,
		)
	}
}

func TestCollectAllKeepsSuccessfulSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = w.Write([]byte(
			"vless://11111111-1111-1111-1111-111111111111@example.com:443",
		))
	}))
	defer server.Close()

	collector := NewCollector()

	collections, err := collector.CollectAll(
		context.Background(),
		[]Source{
			{
				ID:      "working",
				URL:     server.URL,
				Enabled: true,
			},
			{
				ID:      "broken",
				URL:     "http://127.0.0.1:1",
				Enabled: true,
			},
		},
	)

	if err == nil {
		t.Fatal("expected combined error")
	}

	if len(collections) != 1 {
		t.Fatalf(
			"expected 1 successful collection, got %d",
			len(collections),
		)
	}

	if collections[0].Source.ID != "working" {
		t.Fatalf(
			"expected working source, got %q",
			collections[0].Source.ID,
		)
	}
}

func TestMergeConfigurationsDeduplicates(t *testing.T) {
	cfg1 := config.Config{
		Type:     config.TypeVLESS,
		Address:  "example.com",
		Port:     443,
		UUID:     "11111111-1111-1111-1111-111111111111",
		Network:  "tcp",
	}

	cfg2 := cfg1

	cfg3 := cfg1
	cfg3.Address = "other.example.com"

	collections := []Collection{
		{
			Source: Source{
				ID:      "source-a",
				Enabled: true,
			},
			Configurations: []config.Config{
				cfg1,
				cfg2,
			},
		},
		{
			Source: Source{
				ID:      "source-b",
				Enabled: true,
			},
			Configurations: []config.Config{
				cfg3,
			},
		},
	}

	result := MergeConfigurations(collections)

	if len(result) != 2 {
		t.Fatalf(
			"expected 2 unique configurations, got %d",
			len(result),
		)
	}
}

func TestMergeConfigurationsRejectsInvalidConfigurations(t *testing.T) {
	invalid := config.Config{
		Type:    config.TypeVLESS,
		Address: "example.com",
		Port:    443,
	}

	result := MergeConfigurations([]Collection{
		{
			Source: Source{
				ID:      "test",
				Enabled: true,
			},
			Configurations: []config.Config{
				invalid,
			},
		},
	})

	if len(result) != 0 {
		t.Fatalf(
			"expected invalid configuration to be removed, got %d",
			len(result),
		)
	}
}
