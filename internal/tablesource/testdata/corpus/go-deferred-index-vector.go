package entities

//ptah:schema:table name="items"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
	//ptah:schema:index name="by_body" fields="body" type="vector" distance="cosine" vector_type="float" vector_dimension="4" levels="2" clusters="8"
	Other string
}
