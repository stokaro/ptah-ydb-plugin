package sourceencode

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ttlgrammar"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Table is one table of an encoding request: its binding, and the facets of
// it the request asks to write.
type Table struct {
	Binding *protocol.SourceBinding
	Values  []*protocol.EncodeSourceValue
}

// TableResult is how the facets of one table are written: Written are the IDs
// of the facets the parts write together, and Losses holds one line for each
// facet nothing writes.
type TableResult struct {
	Written []string
	Parts   []*protocol.EncodedSourcePart
	Losses  []*protocol.EncodeLoss
}

// tableFacets are the facets of one table as a source declares them.
type tableFacets struct {
	binding *protocol.SourceBinding
	// row and storage are the row-table settings and the column-table
	// storage, with the IDs of their values.
	row              *native.RowTableIntent
	rowID, storageID string
	storage          *native.ColumnStorageIntent
	// feeds are the table's changefeeds, feedsID the value that holds them.
	feeds   []changefeeds.Feed
	feedsID string
	// columns are the columns a family holds, in the order of the request.
	columns []placedColumn
	// neutral are the facets a source states by writing nothing: an empty
	// native index set, and a column the key's family holds.
	neutral []string
	losses  []*protocol.EncodeLoss
}

// placedColumn is a column a declared family holds.
type placedColumn struct{ id, name, family string }

// EncodeTable writes the facets of one table, in YAML or Go, as the table
// declaration the provider's table decoders read: the row table's
// partitioning, read replicas, key Bloom filter, starting layout and TTL as
// keys of the table entry or attributes of its ptah:schema:table directive;
// its column families, each with the columns it holds, under column_families
// or as ptah:schema:columnfamily directives; a column table's storage under
// column_store or as the directive's attributes; and its changefeeds with
// their consumers. The primary key a value repeats is the common columns',
// which the host writes. A column default is the common column's too.
//
// A facet a source states by leaving it out is written by the table's own
// part: the table directive in Go, the column_families key in YAML. That is an
// empty native index set, an empty changefeed set and a column that sits in
// the default family.
//
// What no declaration states is a loss for its facet: an access requirement,
// table attributes, storage settings, tiering, a temporary table, a family
// kept in memory, a TTL run interval, a disabled changefeed or one an async
// replication owns, a native index, a column's sequence, compression or
// encoding. The parts are decoded again before they are returned, and a facet
// they do not give back unchanged is a loss.
func EncodeTable(format protocol.SourceDocument_Format, table Table) (TableResult, error) {
	if format != protocol.SourceDocument_YAML && format != protocol.SourceDocument_GO_ANNOTATIONS {
		return TableResult{}, fmt.Errorf("unsupported table source format")
	}
	facets := &tableFacets{binding: table.Binding}
	for _, value := range table.Values {
		if err := facets.add(value); err != nil {
			return TableResult{}, err
		}
	}
	facets.dropPlacementsWithoutFamilies()
	// A facet the decoders do not give back is lost, and the others are
	// written again without it; each round loses at least one facet, so the
	// loop ends.
	for {
		parts := facets.parts(format)
		lost, err := facets.verify(format, parts)
		if err != nil {
			return TableResult{}, err
		}
		if len(lost) == 0 {
			return facets.result(parts), nil
		}
		for _, id := range lost {
			facets.lose(id, "table %s: its YDB settings cannot be written so that a source reads them back unchanged", tableSubject(table.Binding))
		}
	}
}

func tableSubject(binding *protocol.SourceBinding) string {
	return Subject(binding.GetObject())
}

// add reads one facet of the table, or records why a source cannot state it.
func (f *tableFacets) add(value *protocol.EncodeSourceValue) error {
	envelope := value.GetValue().GetValue()
	id := value.GetValue().GetId()
	normalized, err := schema.NormalizeDesired(envelope)
	if err != nil {
		return err
	}
	switch envelope.GetKind() {
	case schema.TableProperties:
		var intent native.RowTableIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return err
		}
		if setting := rowLoss(&intent); setting != "" {
			f.lose(id, "table %s: %s", tableSubject(f.binding), setting)
			return nil
		}
		f.row, f.rowID = &intent, id
	case schema.ColumnStorageProperties:
		var intent native.ColumnStorageIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return err
		}
		f.storage, f.storageID = &intent, id
	case schema.ColumnProperties:
		var intent native.ColumnIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return err
		}
		owner := envelope.GetOwner()
		switch {
		case intent.GetSequence() != nil:
			f.lose(id, "column %s: its sequence has no source spelling", Subject(owner))
		case intent.Compression != nil || len(intent.GetEncoding()) != 0:
			f.lose(id, "column %s: its compression or encoding has no source spelling", Subject(owner))
		case intent.GetFamily() == "" || intent.GetFamily() == defaultFamily:
			// The default family holds every column no other family names,
			// and a literal default is the common column's.
			f.neutral = append(f.neutral, id)
		default:
			f.columns = append(f.columns, placedColumn{id: id, name: owner.GetName(), family: intent.GetFamily()})
		}
	case schema.ChangefeedProperties:
		var intent native.ChangefeedSetIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return err
		}
		feeds, reason := declaredFeeds(&intent)
		if reason != "" {
			f.lose(id, "table %s: its changefeeds have no source spelling: %s", tableSubject(f.binding), reason)
			return nil
		}
		f.feeds, f.feedsID = feeds, id
	case schema.IndexProperties:
		var intent native.IndexSetIntent
		if err := protojson.Unmarshal(normalized.GetPayload(), &intent); err != nil {
			return err
		}
		if len(intent.GetIndexes()) == 0 {
			f.neutral = append(f.neutral, id)
			return nil
		}
		names := make([]string, 0, len(intent.GetIndexes()))
		for _, index := range intent.GetIndexes() {
			names = append(names, index.GetSpecification().GetName())
		}
		f.lose(id, "table %s: its native indexes (%s) have no source spelling the provider writes", tableSubject(f.binding), strings.Join(names, ", "))
	default:
		f.lose(id, "table %s: its %s have no source spelling", tableSubject(f.binding), strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/"), "-", " "))
	}
	return nil
}

// defaultFamily is the family every row table has.
const defaultFamily = "default"

// rowLoss says which setting of a row table no source declares, the first
// one it finds.
func rowLoss(intent *native.RowTableIntent) string {
	switch {
	case intent.GetAccess() != nil:
		return "its access requirement has no source spelling"
	case len(intent.GetAttributes()) != 0:
		return "its attributes have no source spelling"
	case intent.GetStorageSettings() != nil:
		return "its storage settings have no source spelling"
	case intent.GetTiering() != "":
		return "its tiering has no source spelling"
	case intent.GetTemporary():
		return "its temporary lifetime has no source spelling"
	case len(intent.GetPartitioningSettings().GetPartitionBy()) != 0:
		return "its partition columns have no source spelling"
	case intent.GetTtlSettings().GetRunIntervalSeconds() != 0:
		return "its TTL run interval has no source spelling"
	}
	for _, family := range intent.GetColumnFamilies() {
		if family.GetKeepInMemory() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
			return "the cache mode of its column family " + family.GetName() + " has no source spelling"
		}
	}
	return ""
}

// declaredFeeds reads a changefeed set as the declarations the changefeed
// comparison reads it as, or says why one of them has none.
func declaredFeeds(intent *native.ChangefeedSetIntent) ([]changefeeds.Feed, string) {
	var feeds []changefeeds.Feed
	for _, changefeed := range intent.GetChangefeeds() {
		feed, err := changefeeds.FromIntent(changefeed)
		switch {
		case err != nil:
			return nil, err.Error()
		case feed.Disabled:
			return nil, fmt.Sprintf("changefeed %q is disabled, and no statement disables one", feed.Name)
		case feed.Replicated:
			return nil, fmt.Sprintf("changefeed %q is the stream of an async replication, which a source does not declare", feed.Name)
		}
		feeds = append(feeds, feed)
	}
	return feeds, ""
}

// lose records a facet nothing writes and forgets the declaration it held.
func (f *tableFacets) lose(id, format string, arguments ...any) {
	f.losses = append(f.losses, &protocol.EncodeLoss{ValueId: id, Message: fmt.Sprintf(format, arguments...)})
	switch id {
	case f.rowID:
		f.row, f.rowID = nil, ""
		f.dropPlacementsWithoutFamilies()
	case f.storageID:
		f.storage, f.storageID = nil, ""
	case f.feedsID:
		f.feeds, f.feedsID = nil, ""
	}
	f.columns = slices.DeleteFunc(f.columns, func(column placedColumn) bool { return column.id == id })
	f.neutral = slices.DeleteFunc(f.neutral, func(neutral string) bool { return neutral == id })
}

// dropPlacementsWithoutFamilies loses a column whose family the table does not
// declare: the column is placed by its family's declaration.
func (f *tableFacets) dropPlacementsWithoutFamilies() {
	for _, column := range slices.Clone(f.columns) {
		if f.row == nil || !slices.ContainsFunc(f.row.GetColumnFamilies(), func(family *Ydb_Table.ColumnFamily) bool { return family.GetName() == column.family }) {
			f.lose(column.id, "column %s/%s sits in family %s, which no family the source declares for the table holds", tableSubject(f.binding), column.name, column.family)
		}
	}
}

// written are the IDs the parts write, in a fixed order.
func (f *tableFacets) written() []string {
	var ids []string
	for _, id := range []string{f.rowID, f.storageID, f.feedsID} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	for _, column := range f.columns {
		ids = append(ids, column.id)
	}
	return append(ids, f.neutral...)
}

func (f *tableFacets) result(parts []*protocol.EncodedSourcePart) TableResult {
	result := TableResult{Written: f.written(), Losses: f.losses}
	if len(result.Written) != 0 {
		result.Parts = parts
	}
	return result
}

// parts writes the declared facets in a format.
func (f *tableFacets) parts(format protocol.SourceDocument_Format) []*protocol.EncodedSourcePart {
	if len(f.written()) == 0 {
		return nil
	}
	if format == protocol.SourceDocument_YAML {
		return f.yamlParts()
	}
	return f.goParts()
}

// fragmentID names a part of the table's group.
func (f *tableFacets) fragmentID(suffix string) string {
	return "table." + f.binding.GetId() + "." + suffix
}

// tableAttributes are the attributes a Go source writes on the table
// directive, and the scalar keys of a YAML table entry, in the order the
// decoder reads them: partitioning, starting layout, column storage, TTL.
func (f *tableFacets) tableAttributes(format protocol.SourceDocument_Format) []*protocol.SourceEntry {
	var entries []*protocol.SourceEntry
	add := func(key, value string) { entries = append(entries, field(key, value)) }
	if row := f.row; row != nil {
		partitioning := row.GetPartitioningSettings()
		addSwitch(add, tablesource.AttributeBySize, partitioning.GetPartitioningBySize())
		addCount(add, tablesource.AttributePartitionSizeMB, partitioning.GetPartitionSizeMb())
		addSwitch(add, tablesource.AttributeByLoad, partitioning.GetPartitioningByLoad())
		addCount(add, tablesource.AttributeMinPartitions, partitioning.GetMinPartitionsCount())
		addCount(add, tablesource.AttributeMaxPartitions, partitioning.GetMaxPartitionsCount())
		if replicas := row.GetReadReplicasSettings(); replicas != nil {
			add(tablesource.AttributeReadReplicas, readReplicas(replicas))
		}
		addSwitch(add, tablesource.AttributeKeyBloomFilter, row.GetKeyBloomFilter())
		addCount(add, tablesource.AttributeUniformPartitions, row.GetUniformPartitions())
		if len(row.GetPartitionAtKeys()) != 0 {
			add(tablesource.AttributePartitionAtKeys, splitPoints(row.GetPartitionAtKeys()))
		}
	}
	if storage := f.storage; storage != nil && format == protocol.SourceDocument_GO_ANNOTATIONS {
		add(tablesource.AttributeStore, "COLUMN")
		if len(storage.GetHashColumns()) != 0 {
			add(tablesource.AttributeHash, strings.Join(storage.GetHashColumns(), ","))
		}
		addCount(add, tablesource.AttributeShards, storage.GetPartitions())
		if tiered := tieredTTL(storage); tiered != "" {
			add(tablesource.AttributeColumnTTL, tiered)
		}
	}
	if format == protocol.SourceDocument_GO_ANNOTATIONS {
		for _, setting := range f.ttl() {
			add(tablesource.PlatformPrefix+setting.Key, setting.GetValue().GetText())
		}
	}
	return entries
}

func addSwitch(add func(key, value string), key string, status Ydb.FeatureFlag_Status) {
	switch status {
	case Ydb.FeatureFlag_ENABLED:
		add(key, "ENABLED")
	case Ydb.FeatureFlag_DISABLED:
		add(key, "DISABLED")
	}
}

// addCount writes a count, which the decoder takes as at least 1; zero is a
// count the value leaves unset.
func addCount(add func(key, value string), key string, count uint64) {
	if count != 0 {
		add(key, strconv.FormatUint(count, 10))
	}
}

func readReplicas(settings *Ydb_Table.ReadReplicasSettings) string {
	if count, perZone := settings.GetSettings().(*Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount); perZone {
		return "PER_AZ:" + strconv.FormatUint(count.PerAzReadReplicasCount, 10)
	}
	return "ANY_AZ:" + strconv.FormatUint(settings.GetAnyAzReadReplicasCount(), 10)
}

// splitPoints writes split points in the grammar the decoder reads: each
// value quoted, a point of several values in parentheses.
func splitPoints(points []*native.SplitPoint) string {
	quote := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	written := make([]string, 0, len(points))
	for _, point := range points {
		values := make([]string, 0, len(point.GetValues()))
		for _, value := range point.GetValues() {
			values = append(values, "'"+quote.Replace(value)+"'")
		}
		if len(values) == 1 {
			written = append(written, values[0])
		} else {
			written = append(written, "("+strings.Join(values, ", ")+")")
		}
	}
	return strings.Join(written, ", ")
}

// unitNames spells a TTL's epoch unit as the decoder reads it, by the native
// number both TTL forms count units with.
var unitNames = map[uint64]string{1: ttlgrammar.Seconds, 2: ttlgrammar.Milliseconds, 3: ttlgrammar.Microseconds, 4: ttlgrammar.Nanoseconds}

// ttl is the TTL that only deletes, as the row_deletion properties spell it:
// a row table's TTL, or a column table's TTL of one deleting tier.
func (f *tableFacets) ttl() []*protocol.SourceEntry {
	var column, unit string
	var seconds uint64
	switch {
	case f.row.GetTtlSettings() != nil:
		settings := f.row.GetTtlSettings()
		if mode := settings.GetDateTypeColumn(); mode != nil {
			column, seconds = mode.GetColumnName(), uint64(mode.GetExpireAfterSeconds())
		} else {
			mode := settings.GetValueSinceUnixEpoch()
			column, seconds, unit = mode.GetColumnName(), uint64(mode.GetExpireAfterSeconds()), unitNames[uint64(mode.GetColumnUnit())]
		}
	case f.storage != nil && deletesOnly(f.storage):
		enabled := f.storage.GetTtl().GetEnabled()
		column, seconds, unit = enabled.GetColumn(), enabled.GetTiers()[0].GetApplyAfterSeconds(), unitNames[enabled.GetColumnUnit()]
	default:
		return nil
	}
	settings := []*protocol.SourceEntry{field("row_deletion_column", column), field("row_deletion_interval", ydbdecl.FormatInterval(seconds))}
	if unit != "" {
		settings = append(settings, field("row_deletion_unit", unit))
	}
	return settings
}

// deletesOnly reports a column table's TTL of one tier that deletes.
func deletesOnly(storage *native.ColumnStorageIntent) bool {
	tiers := storage.GetTtl().GetEnabled().GetTiers()
	return len(tiers) == 1 && tiers[0].GetDelete() != nil
}

// columnTTL is the column_ttl attribute's object.
type columnTTL struct {
	Column string       `json:"column"`
	Unit   string       `json:"unit,omitempty"`
	Tiers  []columnTier `json:"tiers"`
}

type columnTier struct {
	Interval       string `json:"interval"`
	ExternalSource string `json:"external_source,omitempty"`
}

// tieredTTL is a column table's TTL that moves rows, as the column_ttl
// attribute writes it, or empty for none.
func tieredTTL(storage *native.ColumnStorageIntent) string {
	enabled := storage.GetTtl().GetEnabled()
	if enabled == nil || deletesOnly(storage) {
		return ""
	}
	written := columnTTL{Column: enabled.GetColumn(), Unit: unitNames[enabled.GetColumnUnit()]}
	for _, tier := range enabled.GetTiers() {
		written.Tiers = append(written.Tiers, columnTier{Interval: ydbdecl.FormatInterval(tier.GetApplyAfterSeconds()), ExternalSource: tier.GetExternalStorage()})
	}
	// The object holds strings and a slice, so encoding cannot fail.
	encoded, _ := json.Marshal(written)
	return string(encoded)
}

// families are the declared families with the columns each holds.
func (f *tableFacets) families() []family {
	var families []family
	for _, declared := range f.row.GetColumnFamilies() {
		written := family{name: declared.GetName()}
		if data := declared.GetData(); data != nil {
			written.data = data.GetMedia()
		}
		switch declared.GetCompression() {
		case Ydb_Table.ColumnFamily_COMPRESSION_NONE:
			written.compression = "off"
		case Ydb_Table.ColumnFamily_COMPRESSION_LZ4:
			written.compression = "lz4"
		}
		for _, column := range f.columns {
			if column.family == declared.GetName() {
				written.fields = append(written.fields, column.name)
			}
		}
		families = append(families, written)
	}
	return families
}

type family struct {
	name, data, compression string
	fields                  []string
}

func (f *tableFacets) goParts() []*protocol.EncodedSourcePart {
	claims := goClaims()
	part := func(suffix string, claim *protocol.GoSourceDirective, entries []*protocol.SourceEntry) *protocol.EncodedSourcePart {
		return &protocol.EncodedSourcePart{Fragment: &protocol.SourceFragment{Id: f.fragmentID(suffix), Selector: claim.GetSelector(), GoDirective: proto.CloneOf(claim),
			BindingId: f.binding.GetId(), Content: &protocol.SourceFragment_Value{Value: mapping(entries...)}}}
	}
	parts := []*protocol.EncodedSourcePart{part("table", claims[tablesource.DirectiveTable], f.tableAttributes(protocol.SourceDocument_GO_ANNOTATIONS))}
	for i, declared := range f.families() {
		entries := []*protocol.SourceEntry{field(tablesource.AttributeName, declared.name)}
		if declared.data != "" {
			entries = append(entries, field(tablesource.AttributeData, declared.data))
		}
		if declared.compression != "" {
			entries = append(entries, field(tablesource.AttributeCompression, declared.compression))
		}
		if len(declared.fields) != 0 {
			entries = append(entries, field(tablesource.AttributeFields, strings.Join(declared.fields, ",")))
		}
		parts = append(parts, part("family."+strconv.Itoa(i), claims[tablesource.DirectiveColumnFamily], entries))
	}
	for i, feed := range f.feeds {
		parts = append(parts, part("changefeed."+strconv.Itoa(i), claims[changefeedsource.DirectiveChangefeed], sortedFields(changefeedValues(feed.Changefeed))))
		for j, consumer := range feed.Consumers {
			values := ydbdecl.ConsumerValues(consumer)
			values["changefeed"] = feed.Name
			parts = append(parts, part("changefeed."+strconv.Itoa(i)+".consumer."+strconv.Itoa(j), claims[changefeedsource.DirectiveConsumer], sortedFields(values)))
		}
	}
	return parts
}

// goClaims are the Go claims of the table decoders, by directive.
func goClaims() map[string]*protocol.GoSourceDirective {
	claims := make(map[string]*protocol.GoSourceDirective)
	for _, claim := range slices.Concat(tablesource.GoDirectives(), changefeedsource.GoDirectives()) {
		claims[claim.GetDirective()] = claim
	}
	return claims
}

// changefeedValues writes a changefeed as the attribute values the decoder
// reads, without its consumers.
func changefeedValues(feed ydbdecl.Changefeed) map[string]string {
	values := map[string]string{ydbdecl.ChangefeedName: feed.Name, ydbdecl.AttributeMode: feed.Mode, ydbdecl.AttributeFormat: feed.Format}
	flags := map[string]bool{ydbdecl.AttributeVirtualTimestamps: feed.VirtualTimestamps, ydbdecl.AttributeInitialScan: feed.InitialScan,
		ydbdecl.AttributeUserSIDs: feed.UserSIDs, ydbdecl.AttributeSchemaChanges: feed.SchemaChanges, ydbdecl.AttributeTopicAutoPartitioning: feed.TopicAutoPartitioning}
	for key, on := range flags {
		if on {
			values[key] = "true"
		}
	}
	if feed.ResolvedTimestamps != "" {
		values[ydbdecl.AttributeResolvedTimestamps] = feed.ResolvedTimestamps
	}
	if feed.RetentionPeriod != "" {
		values[ydbdecl.AttributeFeedRetentionPeriod] = feed.RetentionPeriod
	}
	if feed.TopicMinActivePartitions != 0 {
		values[ydbdecl.AttributeTopicMinActivePartitions] = strconv.FormatUint(feed.TopicMinActivePartitions, 10)
	}
	return values
}

func (f *tableFacets) yamlParts() []*protocol.EncodedSourcePart {
	claims := make(map[string]*protocol.YAMLSourceKey)
	for _, claim := range slices.Concat(tablesource.YAMLKeys(), []*protocol.YAMLSourceKey{changefeedsource.YAMLKey()}) {
		claims[claim.GetKey()] = claim
	}
	part := func(key string, value *protocol.SourceValue) *protocol.EncodedSourcePart {
		claim := claims[key]
		return &protocol.EncodedSourcePart{Fragment: &protocol.SourceFragment{Id: f.fragmentID(key), Selector: claim.GetSelector(), YamlKey: proto.CloneOf(claim),
			BindingId: f.binding.GetId(), Content: &protocol.SourceFragment_Value{Value: value}}}
	}
	var parts []*protocol.EncodedSourcePart
	for _, entry := range f.tableAttributes(protocol.SourceDocument_YAML) {
		parts = append(parts, part(entry.GetKey(), entry.GetValue()))
	}
	if ttl := f.ttl(); len(ttl) != 0 {
		parts = append(parts, part(tablesource.KeyPlatformYDB, mapping(ttl...)))
	}
	if f.storage != nil {
		parts = append(parts, part(tablesource.KeyColumnStore, yamlColumnStore(f.storage)))
	}
	// The families key declares the families, and it is where a column of the
	// default family and an empty native index set are stated.
	if declared := f.families(); len(declared) != 0 || len(f.neutral) != 0 {
		var families []*protocol.SourceEntry
		for _, declared := range declared {
			var settings []*protocol.SourceEntry
			if declared.data != "" {
				settings = append(settings, field(tablesource.AttributeData, declared.data))
			}
			if declared.compression != "" {
				settings = append(settings, field(tablesource.AttributeCompression, declared.compression))
			}
			if len(declared.fields) != 0 {
				var names []*protocol.SourceValue
				for _, name := range declared.fields {
					names = append(names, text(name))
				}
				settings = append(settings, &protocol.SourceEntry{Key: tablesource.AttributeFields,
					Value: &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: names}}}})
			}
			families = append(families, &protocol.SourceEntry{Key: declared.name, Value: mapping(settings...)})
		}
		parts = append(parts, part(tablesource.KeyColumnFamilies, mapping(families...)))
	}
	if f.feedsID != "" {
		var feeds []*protocol.SourceEntry
		for _, feed := range f.feeds {
			values := changefeedValues(feed.Changefeed)
			delete(values, ydbdecl.ChangefeedName)
			options := sortedFields(values)
			if len(feed.Consumers) != 0 {
				var consumers []*protocol.SourceEntry
				for _, consumer := range feed.Consumers {
					settings := ydbdecl.ConsumerValues(consumer)
					delete(settings, ydbdecl.TopicName)
					consumers = append(consumers, &protocol.SourceEntry{Key: consumer.Name, Value: mapping(sortedFields(settings)...)})
				}
				options = append(options, &protocol.SourceEntry{Key: "consumers", Value: mapping(consumers...)})
			}
			feeds = append(feeds, &protocol.SourceEntry{Key: feed.Name, Value: mapping(options...)})
		}
		parts = append(parts, part(changefeedsource.KeyChangefeeds, mapping(feeds...)))
	}
	return parts
}

// yamlColumnStore writes a column table's storage as the column_store key
// reads it. A TTL that only deletes is written with the row_deletion keys.
func yamlColumnStore(storage *native.ColumnStorageIntent) *protocol.SourceValue {
	var entries []*protocol.SourceEntry
	if len(storage.GetHashColumns()) != 0 {
		var names []*protocol.SourceValue
		for _, name := range storage.GetHashColumns() {
			names = append(names, text(name))
		}
		entries = append(entries, &protocol.SourceEntry{Key: "hash_columns", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: names}}}})
	}
	if storage.GetPartitions() != 0 {
		entries = append(entries, &protocol.SourceEntry{Key: "partitions", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Number{Number: strconv.FormatUint(storage.GetPartitions(), 10)}}})
	}
	if enabled := storage.GetTtl().GetEnabled(); enabled != nil && !deletesOnly(storage) {
		ttl := []*protocol.SourceEntry{field("column", enabled.GetColumn())}
		if unit := unitNames[enabled.GetColumnUnit()]; unit != "" {
			ttl = append(ttl, field("unit", unit))
		}
		var tiers []*protocol.SourceValue
		for _, tier := range enabled.GetTiers() {
			written := []*protocol.SourceEntry{field("interval", ydbdecl.FormatInterval(tier.GetApplyAfterSeconds()))}
			if source := tier.GetExternalStorage(); source != "" {
				written = append(written, field("external_source", source))
			}
			tiers = append(tiers, mapping(written...))
		}
		ttl = append(ttl, &protocol.SourceEntry{Key: "tiers", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: tiers}}}})
		entries = append(entries, &protocol.SourceEntry{Key: "ttl", Value: mapping(ttl...)})
	}
	return mapping(entries...)
}

// verify decodes the parts with the table decoders and returns the IDs of the
// facets they do not give back unchanged.
func (f *tableFacets) verify(format protocol.SourceDocument_Format, parts []*protocol.EncodedSourcePart) ([]string, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	document := &protocol.SourceDocument{Id: "encoded", Format: format, Bindings: []*protocol.SourceBinding{proto.CloneOf(f.binding)}}
	for _, part := range parts {
		document.Fragments = append(document.Fragments, part.GetFragment())
	}
	// The table decoder and the changefeed decoder each refuse a document as
	// a whole; a refusal loses the facets that decoder reads.
	var lost []string
	feeds, err := changefeedsource.Decode(context.Background(), document)
	if err != nil && f.feedsID != "" {
		lost = append(lost, f.feedsID)
		feeds = nil
	}
	tables, err := tablesource.Decode(context.Background(), document)
	if err != nil {
		return append(lost, f.tableSourceIDs()...), nil
	}
	var decodedRow *native.RowTableIntent
	var decodedStorage *native.ColumnStorageIntent
	placed := map[string]string{}
	for _, table := range tables {
		switch table.Value.GetValue().GetKind() {
		case schema.TableProperties:
			decodedRow = &native.RowTableIntent{}
			if err := protojson.Unmarshal(table.Value.GetValue().GetPayload(), decodedRow); err != nil {
				return nil, err
			}
		case schema.ColumnStorageProperties:
			decodedStorage = &native.ColumnStorageIntent{}
			if err := protojson.Unmarshal(table.Value.GetValue().GetPayload(), decodedStorage); err != nil {
				return nil, err
			}
		}
		for _, column := range table.Columns {
			var intent native.ColumnIntent
			if err := protojson.Unmarshal(column.GetValue().GetPayload(), &intent); err != nil {
				return nil, err
			}
			placed[column.GetValue().GetOwner().GetName()] = intent.GetFamily()
		}
	}
	if f.row != nil {
		want := proto.CloneOf(f.row)
		want.PrimaryKey = nil
		if !sameIntent(want, decodedRow, &native.RowTableIntent{}) {
			lost = append(lost, f.rowID)
		}
	}
	if f.storage != nil && !proto.Equal(f.storage, decodedStorage) {
		lost = append(lost, f.storageID)
	}
	for _, column := range f.columns {
		if placed[column.name] != column.family {
			lost = append(lost, column.id)
		}
	}
	if f.feedsID != "" && !slices.Contains(lost, f.feedsID) && !sameFeeds(f.feeds, feeds) {
		lost = append(lost, f.feedsID)
	}
	return lost, nil
}

// sameIntent reports a decoded intent equal to the wanted one, an absent one
// standing for the empty intent a source states by writing nothing.
func sameIntent[M proto.Message](want, got M, empty M) bool {
	if reflect.ValueOf(got).IsNil() {
		return proto.Equal(want, empty)
	}
	return proto.Equal(want, got)
}

// sameFeeds reports decoded changefeeds equal to the wanted ones, by the
// declarations the changefeed comparison reads.
func sameFeeds(want []changefeeds.Feed, decoded []changefeedsource.Table) bool {
	var got []changefeeds.Feed
	for _, table := range decoded {
		var set native.ChangefeedSetIntent
		if err := protojson.Unmarshal(table.Value.GetValue().GetPayload(), &set); err != nil {
			return false
		}
		feeds, reason := declaredFeeds(&set)
		if reason != "" {
			return false
		}
		got = append(got, feeds...)
	}
	return reflect.DeepEqual(want, got) || (len(want) == 0 && len(got) == 0)
}

// tableSourceIDs are the facets the table decoder reads, all lost when it
// refuses the parts.
func (f *tableFacets) tableSourceIDs() []string {
	var ids []string
	for _, id := range []string{f.rowID, f.storageID} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	for _, column := range f.columns {
		ids = append(ids, column.id)
	}
	return ids
}
