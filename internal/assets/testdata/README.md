# LINE audio regression fixture

`line-transcoded-tone.m4a` is a synthetic two-second test tone. A generated WAV fixture was manually sent to the controlled LINE test group on 2026-09-05; LINE returned these 5,312 bytes as `audio/x-m4a`. The payload contains one AAC audio stream with `isom/iso2/mp41` container brands. It contains no speech or user recording.

The service regression test checks completion validation and scan-pending state using these exact bytes.
