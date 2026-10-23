package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="cold"
//ptah:schema:columnfamily name="cold" data="hdd"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
