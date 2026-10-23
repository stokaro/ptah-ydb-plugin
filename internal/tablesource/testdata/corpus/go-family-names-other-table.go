package entities

//ptah:schema:table name="items"
//ptah:schema:columnfamily name="cold" table="shop.orders" compression="lz4"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}

//ptah:schema:table name="orders" schema="shop"
type Order struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
