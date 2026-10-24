package entities

//ptah:schema:table name="items" store="COLUMN" partition_by_hash="id" column_ttl="{\"column\":\"expires\",\"unit\":\"SECONDS\",\"tiers\":[{\"interval\":\"PT1H\",\"external_source\":\"/local/ext/bucket\"},{\"interval\":\"P1D\"}]}"
type Item struct {
	//ptah:schema:field name="expires" type="BIGINT" primary="true"
	Expires int64
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
