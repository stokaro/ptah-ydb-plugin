package entities

//ptah:schema:table name="items" auto_partitioning_by_load="ENABLED" key_bloom_filter="ENABLED" uniform_partitions="8"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
