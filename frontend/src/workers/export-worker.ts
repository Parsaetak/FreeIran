/**
 * CSV export worker.
 *
 * Exporting tens of thousands of configurations must never block the
 * UI thread; serialisation happens here and the main thread only
 * receives the finished blob.
 */

interface ExportMessage {
  type: "export";
  rows: Array<Record<string, unknown>>;
}

function csvEscape(value: unknown): string {
  const text = value === null || value === undefined ? "" : String(value);

  if (/[",\n\r]/.test(text)) {
    return `"${text.replace(/"/g, '""')}"`;
  }

  return text;
}

function toCSV(rows: Array<Record<string, unknown>>): string {
  if (rows.length === 0) return "";

  const columns = Object.keys(rows[0]);

  const lines = [columns.join(",")];

  for (const row of rows) {
    lines.push(columns.map((column) => csvEscape(row[column])).join(","));
  }

  return lines.join("\r\n");
}

self.onmessage = (event: MessageEvent<ExportMessage>) => {
  if (event.data?.type !== "export") return;

  const csv = toCSV(event.data.rows);

  (self as unknown as Worker).postMessage({ type: "export:done", csv });
};
