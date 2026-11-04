// Delimited text, .csv and .tsv, shown as the one sheet of a workbook, every
// field as the text it is: nothing is taken for a number, a date or a formula.

import { openSheet } from "./xlsx.js";

export function open(host, source, ui) {
  return openSheet(host, source, ui, source.name.toLowerCase().endsWith(".tsv") ? "tsv" : "csv");
}
