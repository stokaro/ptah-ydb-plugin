---
title: YDB objects for relational readers
description: The YDB objects a schema can declare beside tables, what each one is, what it needs, and which comparison of the provider owns it.
type: concept
audience:
  - schema-author
  - database-engineer
readerQuestion: "I know relational databases but not YDB: what are topics, consumers, changefeeds, transfers and async replications, and how do they depend on tables?"
goal: "Recognize each YDB object a schema can declare, what it needs in order to work, and which comparison owns it."
sourceOfTruth:
  - schema
  - internal/tablecompare
  - internal/topiccompare
  - internal/replicationcompare
  - stokaro/ptah#5807
generated: false
searchAliases:
  - "YDB primer"
  - "YDB topic"
  - "YDB transfer"
  - "YDB consumer"
overlaps: []
disposition: keep
sidebar:
  order: 2
---

A YDB database holds more than tables. Beside them it keeps streams of
messages, the readers registered on them and processes that move data from a
stream into a table. This page describes the objects the provider can
declare, for a reader who knows relational databases and not YDB. [Declaring
YDB objects](declarations.md) gives the YAML keys and Go directives for each,
and the [README](../README.md) gives the rules the provider applies.

One example runs through the page: a table `orders`, a changefeed `feed` that
publishes its row changes, a transfer `ingest_orders` that reads them, and a
table `orders_archive` the transfer writes.

```text
orders ──row changes──▶ orders/feed ──messages──▶ ingest_orders ──rows──▶ orders_archive
(row table)             (changefeed, with a topic)  (transfer)                (row table)
```

YDB moves the data. Ptah declares, compares and creates the four objects, and
YDB runs the transfer after Ptah exits.

## Paths and directories

YDB names every object by a path. The table at `/local/shop/orders` is in the
directory `shop` of the database `/local`. Ptah treats a directory as a
schema: `--schemas shop` selects the objects in `shop`, and the read covers
only the directories a run selects. Some objects live under another object.
The topic of the changefeed `feed` of `orders` is at `shop/orders/feed`.

## Tables

A row table holds rows under a primary key, as a table does in any relational
database. A column table stores its data by column, for analytical queries.
Ptah models the columns, keys and indexes of a table as it does for every
engine. Settings only YDB has, such as TTL, partitioning and column families,
are facets of the table, and for a row table the `ydb-row-table-properties`
comparison owns them.

## Topics and consumers

A topic is a persistent, partitioned stream of messages, comparable to a
Kafka topic. A consumer is a named reading position registered on the topic.
It records how far one reader has read; it is not the program that reads. A
standalone topic is declared under the document key `topics`, with its
consumers, and the `ydb-topics` comparison owns it.

## Changefeeds

A changefeed publishes the row changes of one table to a topic that YDB keeps
under the table. It is declared with its table, under the table's key
`changefeeds`, together with the consumers of its topic. The changefeeds of a
table form one value, the table's complete set, which the
`ydb-row-table-properties` comparison owns as a facet of the table. A
changefeed exists only with its table: dropping the table drops its
changefeeds.

## Transfers

A transfer is a background process inside YDB. It reads a topic, or the topic
of a changefeed, and writes rows to a row table through a YQL lambda that
turns each message into rows. A schema declares it in YAML under the document
key `transfers`, in Go with the directive `ptah:schema:transfer`, or in YQL,
and the `ydb-transfers` comparison owns it. The provider plans transfers for
YDB 25.2 and later. A local-ydb container describes them only when its
`YDB_GRPC_SERVICES` names `replication`.

## Async replications and secrets

An async replication copies tables from another YDB database into this one
and keeps copying. A table an async replication writes refuses writes and
schema changes, so the read leaves it out of the tables. A secret holds a
credential, such as the password an async replication uses to reach the
other database. The read records a secret's path and never its value. The
`ydb-async-replications` and `ydb-secrets` comparisons own them.

## What each object needs

A plan is checked against these dependencies before any statement runs,
because YDB enforces some of them only after the fact:

| Object | Needs | What YDB does without it | What a plan does |
| --- | --- | --- | --- |
| Changefeed | Its table | Drops the changefeed with the table | Plans the changefeed with its table |
| Transfer | Its source topic or changefeed | Accepts the transfer, then stops it (`Discovery error`) | Refuses a transfer whose source the schema declares neither as a topic nor as a changefeed, unless the read recorded the source without describing it |
| Transfer | Its target table | Refuses to create it (`Path does not exist`) | Refuses a transfer into a table the schema does not declare |
| Topic | Nothing, but a transfer that reads it needs it to stay | Drops the topic and keeps the transfer, which then fails | Refuses to drop a topic that a declared transfer reads |

[Understanding the provider boundary](https://docs.ptah.run/edge/extend/write-a-provider/ydb-walkthrough/)
follows the example through Ptah Core and the provider, and [What a
comparison may conclude](https://docs.ptah.run/edge/extend/write-a-provider/ydb-incomplete-knowledge/)
shows each refusal in the table with the test that measures it.
