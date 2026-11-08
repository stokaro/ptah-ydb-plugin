package schema

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// columnPropertyOmission counts a column only when its captured native
// settings have information that DBML or inspection JSON cannot express.
// Defaults are carried by common columns and reported separately when JSON
// omits them. The default family, disabled compression, and disabled encoding
// add no information to the native property warning.
func columnPropertyOmission(payload []byte, representation protocol.ValueInput_Representation) (uint64, error) {
	if representation == protocol.ValueInput_DESIRED {
		var column native.ColumnIntent
		if err := unmarshalCounted(payload, &column); err != nil {
			return 0, err
		}
		return columnOmission(column.GetFamily(), column.GetCompression(), column.GetEncoding()), nil
	}
	var column native.ColumnMeta
	if err := unmarshalCounted(payload, &column); err != nil {
		return 0, err
	}
	return columnOmission(column.GetFamily(), column.GetCompression(), column.GetEncoding()), nil
}

func columnOmission(family string, compression *native.ColumnCompression, encoding []*native.ColumnEncoding) uint64 {
	if (family != "" && family != defaultFamilyName) || columnCompressionStated(compression) {
		return 1
	}
	for _, setting := range encoding {
		if setting.GetDictionary() != nil {
			return 1
		}
	}
	return 0
}

func columnCompressionStated(compression *native.ColumnCompression) bool {
	if compression == nil {
		return false
	}
	algorithm := compression.GetAlgorithm()
	return (algorithm != native.ColumnCompression_ALGORITHM_UNSPECIFIED && algorithm != native.ColumnCompression_ALGORITHM_OFF) ||
		compression.CompressionLevel != nil
}
