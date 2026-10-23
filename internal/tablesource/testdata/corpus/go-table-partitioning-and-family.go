package entities

//ptah:schema:table name="items" auto_partitioning_by_load="ENABLED"
//ptah:schema:columnfamily name="default" compression="lz4"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
