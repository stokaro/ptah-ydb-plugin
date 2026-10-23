package entities

//ptah:schema:table name="items"
//ptah:schema:changefeed name="updates" mode="UPDATES" format="JSON"
//ptah:schema:changefeed:consumer changefeed="updates" name="billing" important
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
