package entities

//ptah:schema:columnfamily name="cold" data="hdd"
//ptah:schema:table name="items"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
