package entities

//ptah:schema:table name="items" store="COLUMN" column_ttl="{\"column\":\"created_at\",\"tiers\":[{\"interval\":\"P30D\",\"external_source\":\"/local/ext/a\"},{\"interval\":\"P7D\"}]}"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
