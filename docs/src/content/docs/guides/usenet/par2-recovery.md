---
title: Manual PAR2 recovery
description: Recover missing data in an existing plain-file NZB using its original recovery files.
---

PAR2 recovery can reconstruct missing or corrupt pieces of an already imported
file when its recovery set has enough usable parity data. Tessarr verifies the
recovered slices and complete file before publishing any repairs. Saved patches
are used transparently for playback, seeking, prefetch, and availability checks.

## Request a recovery

1. Open **Repair → Recover missing Usenet data (PAR2)**.
2. Search by the NZB's name and select an existing plain file. Use **Continue
   search** when more library entries remain to be scanned.
3. Select the **original NZB** for that file. Completed imports normally remove
   their original source; Tessarr cannot discover missing PAR2 article IDs from
   the saved media map alone. A previously supplied matching NZB can be reused.
4. Select **Start recovery**. Review the job's progress and result on the same
   page. **Cancel recovery** stops queued or running work.

The uploaded NZB must contain the exact saved article map for the selected
file. An unrelated release with a similar filename is rejected. Tessarr saves
a private local copy for restart recovery and retries; it does not send that
copy to another service. The usual authentication and request-origin checks
protect these operations.

Recovery can read the entire recovery set and then reread the target to verify
its complete checksum. Its bandwidth cost can therefore exceed one download
of the file. Recovery runs only when requested; health checks and failed reads
do not start it automatically.

## Supported files

This first version supports completed, existing plain-file NZBs with complete
saved article maps. PAR2 files must be named with a `.par2` extension in the
original NZB. Other files required by the same recovery set must also be listed
in that NZB and resolvable by their original names.

Archived members, encrypted files, merged multipart media layouts, new imports
awaiting admission, oversized recovery sets, and ambiguous layouts are rejected
explicitly. Recovery data must still be retrievable. Loss beyond the available
independent recovery slices cannot be repaired.

There is no `usenet.skip_repair` setting. Scheduled health repair and Arr
replacement remain separate operations.

## Limits and restart behavior

The initial version uses fixed safety limits:

| Resource | Limit |
| --- | --- |
| Running jobs / queued jobs | One / up to eight active and queued combined |
| Original NZB | 16 MiB; 512 source files; 100,000 article references |
| Recovery set | 32 GiB; 256 recoverable files; 32,768 input slices |
| Recovery slice / decoded article | 8 MiB / 4 MiB |
| Recovery rows | Up to 16, reduced when needed to fit the buffer budget |
| Recovery buffer budget | 128 MiB; source maps, application memory, and OS caches also consume memory |
| Decoded bytes read per job | 64 GiB, including retried body transfers |
| Download pace / job deadline | 8 MiB/s / six hours, including time paused |
| Persistent patches | 64 MiB per file; 512 MiB total, plus a bounded temporary object during publication |
| Saved original NZBs | 128 MiB total |
| Job history | 32 recent jobs |

Playback takes priority: recovery waits between articles and solver chunks while
a playback stream is active. In-flight articles finish or time out. Shared NNTP
pools reserve one foreground slot when a provider has multiple connections;
a one-connection account cannot serve two requests simultaneously.

Jobs, uploaded sources, and patches live under `usenet/recovery` in Tessarr's
configuration directory. Back up this directory with the other state: patches
may become the only surviving copy of recovered data. Successful patches are
not automatically evicted to make room. Removing an NZB also cancels its
recovery jobs and removes its saved recovery data.

Interrupted jobs resume after a restart, subject to the original read/deadline
limits and a maximum of three attempts. Cancelled jobs stay cancelled. A small
read allowance is charged on restart to cover progress lost during a crash.
Changing or replacing the saved file map fences off older patches.

## API

All endpoints require the same authentication as the other control APIs:

| Method | Endpoint | Purpose |
| --- | --- | --- |
| GET | `/api/usenet/recovery/targets?q=example&after=...` | Search a bounded page of existing plain files by NZB name; follow `next` as `after` |
| GET | `/api/usenet/recovery/jobs` | Recent jobs and safety limits |
| POST | `/api/usenet/recovery/jobs` | Start or return an already active job for the same file |
| DELETE | `/api/usenet/recovery/jobs/{id}` | Cancel a job |

Start with an original NZB:

```bash
curl -H 'Authorization: Bearer <API_TOKEN>' \
  -F 'nzb_id=<EXISTING_NZB_ID>' \
  -F 'file=Example.mkv' \
  -F 'source=@Example.nzb' \
  http://localhost:8282/api/usenet/recovery/jobs
```

After a matching source has been saved, a JSON request containing only
`nzb_id` and `file` can reuse it. Responses never include the uploaded source
or provider credentials. A missing source returns `source_required`; mismatched
file identity returns `stale`; unsupported layouts, unavailable parity, and
resource limits are reported separately.
