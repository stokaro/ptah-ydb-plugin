package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="cold" data="hdd"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}

//ptah:schema:table name="orders"
//ptah:schema:columnfamily name="cold" compression="lz4"
type Order struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
