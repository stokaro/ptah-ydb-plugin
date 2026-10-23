package entities

//ptah:schema:table name="items" auto_partitioning_by_load="ENABLED"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}

//ptah:schema:table name="orders" platform.ydb.row_deletion_column="placed_at" platform.ydb.row_deletion_interval="P7D" key_bloom_filter="ENABLED"
type Order struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="placed_at" type="TIMESTAMP"
	PlacedAt int64
}
