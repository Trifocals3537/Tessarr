# RAR5 encryption parameter limits

[Issue #31](https://github.com/Trifocals3537/tessarr/issues/31) confirmed that
archive and file-encryption readers accepted unsupported algorithms and KDF
counts above the existing maximum of 24. Unchecked integer shifts in the key
derivation entry point could turn those values into excessive or wrapped work.

Both readers now validate encryption version 0 and a count in `0..24`, including
metadata reads without a password. `DeriveKeys` independently validates the
count before any shift, decrement, or HMAC operation and returns an error for
invalid input. A bounded `uint32` iteration count makes the arithmetic identical
on supported 32-bit and 64-bit builds. Callers propagate derivation errors.

File extra records are read within their declared boundaries. Required salt, IV,
and optional password-check bytes must be present before derivation. A short
record cannot borrow fields from the following record, duplicate encryption
records are rejected, and unknown records are skipped by their exact lengths.
When a file password check is present and a password is supplied, it is verified
before returning usable encryption metadata.

## Validation

- Ten archive/file unsafe-parameter cases reproduced before the fix without
  executing costly derivation. Supported bounds remained accepted controls.
- Every possible KDF byte value is checked in the archive/file parameter paths.
  Unsupported versions, negative API counts, large integer inputs, truncation,
  cross-record fields, duplicates, and malformed checks are covered.
- Deadline-bounded worker processes test invalid derivation calls and repeated
  malformed maximum-work records, preventing a regression from hanging tests.
- Independent standard-library PBKDF2 references verify keys, check keys, and
  password-check values at small work factors, including UTF-8 passwords.
- Maximum accepted work is tested through parameter validation and iteration
  scheduling; tests deliberately avoid performing maximum-cost derivation.
- Existing encrypted-header, complete archive, wrong-password, and synthetic
  NNTP article/readback tests retain their coverage.

The count limit is an application policy, retaining the existing value of 24.
It does not make valid derivation instantaneous or add cancellation within the
bounded computation. [Issue #32](https://github.com/Trifocals3537/tessarr/issues/32)
tracks the separate encrypted-header/unencrypted-payload alignment problem.
