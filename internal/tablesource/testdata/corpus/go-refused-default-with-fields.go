package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="default" fields="body"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
