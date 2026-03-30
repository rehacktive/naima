Use for mailbox interaction when the task needs sending an email, checking the inbox, waiting for a confirmation message, or extracting activation/reset links.
Required param: `operation`.
Typical flows:
1) sign-up/send flow: `send`
2) confirmation flow: `list` or `wait` with `after_id` and sender/subject filters
3) inspect the matched email with `get` or use `wait` with `include_body=true`
4) follow returned `links` with `playwright` if activation requires clicking a confirmation URL
