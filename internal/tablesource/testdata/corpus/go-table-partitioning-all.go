package entities

//ptah:schema:table name="items" auto_partitioning_by_size="ENABLED" auto_partitioning_partition_size_mb="2048" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2" auto_partitioning_max_partitions_count="16" read_replicas_settings="ANY_AZ:2" key_bloom_filter="ENABLED"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
