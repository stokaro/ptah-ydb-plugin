package entities

//ptah:schema:table name="items" store="COLUMN" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
