-- Where the session is played. Kept on the session rather than only in
-- settings because a group that books one venue still ends up at a different
-- one occasionally, and the email has to say which.
ALTER TABLE session ADD COLUMN venue TEXT NOT NULL DEFAULT '';

-- Replies to the weekly email should reach the organizer, not the SMTP
-- account the app happens to authenticate as. Mail that a human can reply to
-- also reads as correspondence rather than as a mailshot.
ALTER TABLE email_outbox ADD COLUMN reply_to TEXT NOT NULL DEFAULT '';
