package schema

// SchemaSeparator separates the components of a YDB directory path, which a
// reference carries whole as its schema. A coverage region over a directory's
// subtree covers each schema that is the directory or begins with it and this
// separator.
const SchemaSeparator = "/"
