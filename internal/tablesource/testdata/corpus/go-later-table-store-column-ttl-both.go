package entities

//ptah:schema:table name="items" store="COLUMN" column_ttl="{\"column\":\"created_at\",\"tiers\":[{\"interval\":\"P1D\",\"external_source\":\"/local/ext/bucket\"}]}" platform.ydb.row_deletion_column="created_at" platform.ydb.row_deletion_interval="P30D"
type Item struct {
	//ptah:schema:field name="created_at" type="TIMESTAMP" primary="true"
	CreatedAt int64
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
