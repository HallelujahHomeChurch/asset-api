ALTER TABLE assets
    ADD CONSTRAINT assets_upload_status_check
    CHECK (upload_status IN ('created', 'completed', 'failed')) NOT VALID;

ALTER TABLE assets
    ADD CONSTRAINT assets_scan_status_check
    CHECK (scan_status IN ('pending', 'clean', 'infected', 'failed')) NOT VALID;

ALTER TABLE assets
    ADD CONSTRAINT assets_processing_status_check
    CHECK (processing_status IN ('pending', 'ready', 'not_required', 'failed')) NOT VALID;

ALTER TABLE assets VALIDATE CONSTRAINT assets_upload_status_check;
ALTER TABLE assets VALIDATE CONSTRAINT assets_scan_status_check;
ALTER TABLE assets VALIDATE CONSTRAINT assets_processing_status_check;
