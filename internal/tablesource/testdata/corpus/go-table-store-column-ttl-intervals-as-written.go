package entities

//ptah:schema:table name="items" store="COLUMN" column_ttl="{\"column\":\"created_at\",\"tiers\":[{\"interval\":\"PT3600S\",\"external_source\":\"/local/ext/hot\"},{\"interval\":\"P1W\",\"external_source\":\"/local/ext/cold\"},{\"interval\":\"P30D\"}]}"
type Item struct {
	//ptah:schema:field name="created_at" type="TIMESTAMP" primary="true"
	CreatedAt int64
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
