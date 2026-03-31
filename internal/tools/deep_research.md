Use when the user wants a topic researched end-to-end and the findings stored in the PKB.
Default operation: `create`.
Operations:
1) `create`: queue a persisted background research run
2) `get`: fetch one research run with status/details, optionally logs
3) `list`: list recent research runs
4) `cancel` or `stop`: cancel a queued or running research run
5) `delete`: remove a non-running research run record
Create behavior:
1) stores the run in the database
2) runs the research in background
3) creates or reuses the topic
4) stores the note as the research brief
5) stores selected source pages as documents in the same topic
6) writes a final response document in the same topic with findings and cited sources
Use `get`/`list` to check status later after the page is closed.
