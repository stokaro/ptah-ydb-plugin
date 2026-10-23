package entities

//ptah:schema:table name="items" auto_partitioning_by_size="enabled" auto_partitioning_by_load=" Disabled " read_replicas_settings="per_az:2" key_bloom_filter="disabled"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
