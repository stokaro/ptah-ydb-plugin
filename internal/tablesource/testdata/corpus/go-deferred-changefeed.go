package entities

//ptah:schema:table name="items"
//ptah:schema:changefeed name="updates" mode="UPDATES" format="JSON"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
