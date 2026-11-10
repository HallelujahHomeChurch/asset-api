-- Empty pairs retain the historical namespace policy without a backfill.
ALTER TABLE assets
 ADD COLUMN security_profile text NOT NULL DEFAULT '',
 ADD COLUMN processing_profile text NOT NULL DEFAULT '',
 ADD CONSTRAINT assets_upload_profile_pair CHECK ((security_profile='') = (processing_profile='')) NOT VALID;
