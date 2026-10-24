package entities

//ptah:schema:table name="items" partition_at_keys="10,"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
