package sharding

import (
	"fmt"
	"testing"
)

func TestGetShardID(t *testing.T) {
	tests := []struct {
		entityID string
		want     int
	}{
		{"user-1", 532}, // Corrected values based on crc32.ChecksumIEEE
		{"user-2", 942},
		{"todo-abc", 748},
	}

	for _, tt := range tests {
		t.Run(tt.entityID, func(t *testing.T) {
			if got := GetShardID(tt.entityID); got != tt.want {
				t.Errorf("GetShardID(%q) = %v, want %v", tt.entityID, got, tt.want)
			}
		})
	}
}

func TestGetSubject(t *testing.T) {
	subject := GetSubject("todo", "user-1")
	expected := "app.command.532.todo.user-1"
	if subject != expected {
		t.Errorf("GetSubject = %v, want %v", subject, expected)
	}
}

func TestStableSharding(t *testing.T) {
	// Ensure that the sharding is deterministic and stable
	id := "test-stable-id"
	shard1 := GetShardID(id)
	shard2 := GetShardID(id)

	if shard1 != shard2 {
		t.Errorf("Sharding is not deterministic! %d != %d", shard1, shard2)
	}
}

func TestDistribution(t *testing.T) {
	// Rough check to ensure we don't map everything to shard 0
	distribution := make(map[int]int)
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key-%d", i)
		shard := GetShardID(key)
		distribution[shard]++
	}

	if len(distribution) < 100 {
		t.Errorf("Sharding distribution is too poor. Only %d unique shards used for 1000 keys", len(distribution))
	}
}
