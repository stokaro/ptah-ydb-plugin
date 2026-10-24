package entities

//ptah:schema:table name="items" store="COLUMN" column_ttl="{\"column\":\"expires\",\"unit\":\"milliseconds\",\"tiers\":[{\"interval\":\"PT1H\",\"external_source\":\"/local/ext/bucket\"}]}"
type Item struct {
	//ptah:schema:field name="expires" type="BIGINT" primary="true"
	Expires int64
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
