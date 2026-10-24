package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="cold" fields="body"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
}
