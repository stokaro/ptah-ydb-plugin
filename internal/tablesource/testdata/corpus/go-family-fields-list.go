package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="cold" data="hdd" compression="lz4" fields="body, notes"
//ptah:schema:columnfamily name="warm" fields="title"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
	//ptah:schema:field name="notes" type="TEXT"
	Notes string
	//ptah:schema:field name="title" type="TEXT"
	Title string
}
