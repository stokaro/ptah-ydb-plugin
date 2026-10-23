package entities

//ptah:schema:table name="items"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
	//ptah:schema:index name="by_body" fields="body" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2"
	Other string
}
