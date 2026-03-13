Use to manage personal knowledge topics and documents.
Operations:
- `create_topic`, `list_topics`, `update_topic`, `delete_topic`
- `add_content`, `list_documents`, `temporal_search`, `update_document`, `delete_document`
`add_content` supports URL ingestion (`url`) and notes (`note`).
`temporal_search` supports `timeframe` (`today|week|month`) or custom `since`/`until` and returns an aggregated structured document.
