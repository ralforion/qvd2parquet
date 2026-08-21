package qvd

import "testing"

func TestFieldTags(t *testing.T) {
	const x = `<QvdTableHeader><TableName>T</TableName><Fields>` +
		`<QvdFieldHeader><FieldName>D</FieldName><BitOffset>0</BitOffset><BitWidth>1</BitWidth>` +
		`<NumberFormat><Type>UNKNOWN</Type></NumberFormat><NoOfSymbols>2</NoOfSymbols><Length>5</Length>` +
		`<Tags><String>$numeric</String><String>$timestamp</String></Tags></QvdFieldHeader>` +
		`<QvdFieldHeader><FieldName>N</FieldName><BitOffset>1</BitOffset><BitWidth>1</BitWidth>` +
		`<NumberFormat><Type>UNKNOWN</Type></NumberFormat><NoOfSymbols>2</NoOfSymbols><Length>5</Length>` +
		`<Tags><String>$numeric</String><String>$integer</String></Tags></QvdFieldHeader>` +
		`</Fields><RecordByteSize>1</RecordByteSize><NoOfRecords>1</NoOfRecords></QvdTableHeader>`
	h, err := ParseHeaderXML([]byte(x))
	if err != nil {
		t.Fatal(err)
	}
	cols := h.Columns()
	if !cols[0].HasTag("$timestamp") || !cols[0].HasTag("timestamp") {
		t.Errorf("tags = %v", cols[0].Tags)
	}
	if got, ok := cols[0].TaggedType(); !ok || got != QlikTimestamp {
		t.Errorf("TaggedType = %v, %v; want QlikTimestamp", got, ok)
	}
	if _, ok := cols[1].TaggedType(); ok {
		t.Error("$integer should not yield a date/time type")
	}
}
