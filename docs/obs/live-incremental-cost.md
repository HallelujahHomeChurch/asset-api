# Measure the incremental cost of adding live viewing

Compare the same recordings with live disabled/enabled. The volume below is an
input model, not a bill or a claim of measured provider performance. No current
provider unit price, free allowance or production usage has been verified here.

## Common baseline (do not count twice)

Six gatherings/week × 2.5 hours × 52/12 = 65 hours/month.
1080p 3 Mbps + 720p 1.5 Mbps + 480p 0.8 Mbps, each with 128 kbps audio,
is approximately 5.684 Mbps total, or **166.3 decimal GB/month** before container
and metadata overhead. Both archive-only and live-enabled modes produce those
three final renditions on the Windows machine. Local encoding and final VOD
storage are shared baseline costs.

At 30 seconds per segment, this is about 7,800 segments/profile, or 23,400 closed
media segments/month across three profiles, plus initialization files and short
event tails. A 2.5-hour event has 300 segments/profile; a 12-hour cap has 1,440.

## Incremental live work

| Meter | Capture in A/B evidence | How to attribute the delta |
| --- | --- | --- |
| Recording Job | Billed vCPU-seconds, GiB-seconds, starts, media-copy/probe/decode time | Live closed-segment validation and playlist publication above archive-only processing; include final package validation if still repeated |
| R2 operations | GET/HEAD/LIST/PUT/DELETE and conditional-write retries | Live private copies, revisions/current pointer, playback origin misses and cleanup; subtract archive-only uploads |
| Temporary storage | Byte-hours for staging, live candidates/final copies and revision playlists | Normal temporary copies survive capture expiry plus six-hour writer grace; failed copies have at least a seven-day hold; final VOD is baseline |
| Media Worker | Requests, CPU duration and error/retry rate | Include every authorization/cache lookup even on cache hits |
| CMS/API/DB | Grant/discovery requests, reconciliation and query time/rows | Include scope registrations/renewals and durable automatic publication |
| Existing plan fees | Actual tier and remaining included allowance | Count only fees or overage added by live, not the entire existing subscription |
| Network | Actual charged provider egress by origin and destination | R2-to-viewer policy does not establish Azure or other provider egress pricing |

For scale only, if every viewer watches all 65 live hours and requests one variant
playlist every 15 seconds plus one segment every 30 seconds:

| Simultaneous viewers | Viewer-hours/month | Segment requests | Variant playlist requests | Sum before other requests |
| ---: | ---: | ---: | ---: | ---: |
| 100 | 6,500 | 780,000 | 1,560,000 | 2,340,000 |
| 300 | 19,500 | 2,340,000 | 4,680,000 | 7,020,000 |

This is a request model, not origin R2 requests: cache effectiveness changes origin
reads but does not remove Worker authorization requests. Add master/init requests,
quality switches, retries, late-event playlist bytes and extra DVR viewer-hours.
Playlist poll cadence is an assumption to replace with observed player traffic.
At four-minute grant renewal, the same model adds roughly 97,500 / 292,500 grant
renewals, plus initial scopes, exchanges and visibility recovery. Discovery polling
adds its own API requests.

Report monthly incremental cost as the sum of measured deltas × current verified
unit prices after remaining allowances. Provide currency/date/region and separate
measured figures from extrapolation. A prior small Cloudflare-only estimate is not
an upper bound on the whole system, particularly the existing 4-vCPU/8-GiB Job.
