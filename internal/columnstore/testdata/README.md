# Column-table monitoring captures

These JSON files were copied unchanged from Ptah commit
`7b4b19f49fadf03ce81def2f6168ea60b3261a71`,
`internal/ydbcolumn/testdata/`. They are prior captures from YDB 25.1 and
26.2, not new live qualification of this provider.

They contain schema descriptions, including local indexes omitted by the
table API. The replay tests supply independent table-API column definitions
and reject disagreement before producing a storage observation.
