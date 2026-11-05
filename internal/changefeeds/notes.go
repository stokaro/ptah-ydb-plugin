package changefeeds

import (
	"fmt"
	"strings"
)

// DroppedNote says what dropping a changefeed of table loses, in the host's
// words.
func DroppedNote(table string, changefeed Feed) string {
	note := fmt.Sprintf("Changefeed %s of table %s is dropped with its topic: the records nobody read are lost", changefeed.Name, table)
	consumers := consumerNames(changefeed)
	switch len(consumers) {
	case 0:
		return note + "."
	case 1:
		return note + ", and so is consumer " + consumers[0] + "."
	default:
		return note + ", and so are consumers " + strings.Join(consumers, ", ") + "."
	}
}

// RecreatedNote says why a changefeed of table is dropped and added again, and
// what the restart costs its readers. changefeed is the one dropped.
func RecreatedNote(table string, changefeed Feed) string {
	note := fmt.Sprintf("Changefeed %s of table %s is dropped and added again, because YDB changes no option of a "+
		"changefeed in place. Its stream restarts: the records nobody read are lost", changefeed.Name, table)
	consumers := consumerNames(changefeed)
	switch len(consumers) {
	case 0:
		return note + "."
	case 1:
		return note + ", and consumer " + consumers[0] + " loses its position and starts again from the beginning " +
			"of the new stream."
	default:
		return note + ", and consumers " + strings.Join(consumers, ", ") + " lose their position and start again " +
			"from the beginning of the new stream."
	}
}

// RebuiltNote says why a rebuild of table drops the changefeeds names and adds
// them again once the new table holds its name, and what the restart costs
// their readers, in the host's embedded engine's words. names holds one name
// at least.
func RebuiltNote(table string, names []string) string {
	const why = "because YDB moves no table that carries a changefeed."
	if len(names) == 1 {
		return fmt.Sprintf("Changefeed %s of table %s is dropped before the swap and added again after it, %s Its "+
			"stream restarts: the records nobody read are lost, and its consumers start again from the beginning of "+
			"the new stream.", names[0], table, why)
	}
	return fmt.Sprintf("Changefeeds %s of table %s are dropped before the swap and added again after it, %s Each "+
		"stream restarts: the records nobody read are lost, and the consumers start again from the beginning of "+
		"the new stream.", strings.Join(names, ", "), table, why)
}

// RestartedConsumersNote says why consumers of a changefeed of table are
// dropped and added again.
func RestartedConsumersNote(table, changefeed string, consumers []string) string {
	const why = "because YDB keeps a consumer's codecs once it has any."
	if len(consumers) == 1 {
		return fmt.Sprintf("Consumer %s of changefeed %s of table %s is dropped and added again, %s It loses its "+
			"position and starts again from the beginning of the stream.", consumers[0], changefeed, table, why)
	}
	return fmt.Sprintf("Consumers %s of changefeed %s of table %s are dropped and added again, %s They lose their "+
		"position and start again from the beginning of the stream.", strings.Join(consumers, ", "), changefeed, table, why)
}

// consumerNames lists a changefeed's consumers by name.
func consumerNames(changefeed Feed) []string {
	names := make([]string, len(changefeed.Consumers))
	for i, consumer := range changefeed.Consumers {
		names[i] = consumer.Name
	}
	return names
}
