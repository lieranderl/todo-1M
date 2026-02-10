package sharding

import (
	"fmt"
	"hash/crc32"
)

// ShardCount is the fixed number of partitions for the system.
// We use 1024 as per the architectural constraints.
const ShardCount = 1024

// GetShardID calculates the deterministic shard ID for a given entity ID.
func GetShardID(entityID string) int {
	checksum := crc32.ChecksumIEEE([]byte(entityID))
	return int(checksum % ShardCount)
}

// GetSubject returns the NATS subject for a given entity type and ID.
// Format: app.command.{shard_id}.{entity_type}.{entity_id}
func GetSubject(entityType, entityID string) string {
	shardID := GetShardID(entityID)
	return fmt.Sprintf("app.command.%d.%s.%s", shardID, entityType, entityID)
}
