# Feature-flag pages

Each file is a `/viewer/json/feature_flags?database=/local` page that a
local-ydb container's monitoring endpoint answered, unedited.

- `local-ydb-25.1.4.7.json` and `local-ydb-26.2.1.14.json` were copied
  unchanged from Ptah commit `0c676c27a5997cf5e51928ee7b751b67f2ac6916`,
  `internal/ydbflags/testdata/`. They are the two lines at their default flags.
- `local-ydb-26.2.1.14-tiering-on.json` was read from
  `ydbplatform/local-ydb:26.2.1.14` started with `YDB_FEATURE_FLAGS` set to
  `enable_vector_index,enable_add_unique_index,enable_external_data_sources`,
  after a dynamic configuration turned on `enable_tiering_in_column_shard`
  together with those three flags.
