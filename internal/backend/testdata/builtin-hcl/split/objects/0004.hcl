// ptah:feature-coverage {"format":2,"representation":"desired","kinds":[{"model":{"owner":"ptah.run/ydb","kind":"ptah.run/ydb/coordination-node","representation":"desired","version":1,"definition":"sha256:4c044f15565424fd0212b50fa50c1f760e53c228028c0f0e9ac8399ad3d2183a"},"knowledge":[{"outside":{"state":"complete"}}]}]}

coordination_node "locks" {
  schema = schema.h1
  self_check_period = "PT2S"
  read_consistency_mode = "strict"
}
