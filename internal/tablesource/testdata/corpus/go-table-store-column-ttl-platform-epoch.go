package entities

//ptah:schema:table name="items" store="COLUMN" platform.ydb.row_deletion_column="expires" platform.ydb.row_deletion_interval="PT2H" platform.ydb.row_deletion_unit="seconds"
type Item struct {
	//ptah:schema:field name="expires" type="BIGINT" primary="true"
	Expires int64
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
}
