-- 000005_sent_append_generic: accounts on generic servers added before
-- sending existed get sent_append = true, as new ones do (plan 3.9: such
-- servers do not save sent mail in Sent themselves). Gmail and Office 365
-- keep false.
UPDATE accounts SET sent_append = true WHERE provider = 'generic' AND NOT sent_append;
