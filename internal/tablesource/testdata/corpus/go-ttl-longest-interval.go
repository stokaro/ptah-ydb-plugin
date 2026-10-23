package entities

//ptah:schema:table name="items" platform.ydb.row_deletion_column="created_at" platform.ydb.row_deletion_interval="P49672DT23H59M59S"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
