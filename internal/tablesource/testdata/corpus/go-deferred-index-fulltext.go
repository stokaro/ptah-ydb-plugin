package entities

//ptah:schema:table name="items"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="body" type="TEXT"
	Body string
	//ptah:schema:index name="by_body" fields="body" type="fulltext" tokenizer="standard"
	Other string
}
