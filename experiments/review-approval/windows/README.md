# Windows approval spike

This standalone WinForms app displays one full saved-note or held-proposal
fixture. It uses the native Windows WebAuthn API with platform attachment and
required user verification. The experiment has no server, network requests,
Mora API calls, or vault changes.

## Run the automated checks

Use native 64-bit Windows PowerShell from this directory. The build uses the
inbox .NET Framework compiler and requires no packages or SDK install.

```powershell
.\build.ps1
.\MoraWindowsApprovalSpike.exe --self-test
.\MoraWindowsApprovalSpike.exe --probe
```

The self-test uses a disposable **software** P-256 key and checks saved and
held decisions, single use, fresh challenge, stale preview, changed scope or
action, expiry, missing proof, absent enrollment, Cancel, credential/RP
mismatch, user verification, signature rejection, and enrollment-key parsing.
A native probe reports WebAuthn version, process session, and whether Windows
reports a user-verifying platform authenticator. Neither command opens a
Windows Hello prompt or creates a credential.

## Run on the Windows desktop

Build the executable as above. Launch it from Explorer or use **Win+R**
and enter the full path to `MoraWindowsApprovalSpike.exe` with no arguments.

1. Check the title says **Mora approval spike (synthetic only)** and that Keep
   and Discard start disabled. Click **Cancel** and expect the status
   `Cancelled. No decision recorded.`.
2. Click **Enroll fixture**. In Windows Security choose the option for this
   Windows device if offered a choice. Complete Windows Hello yourself.
   This creates a disposable passkey for `mora-approval-spike.invalid`.
   If Windows offers only another device, cloud sync, a security key, or
   setup of a new sign-in method, cancel and record the prompt shown.
3. On the saved fixture, click **Keep** and cancel the Windows Security
   prompt. Expect `No decision recorded.` and no PASS for that attempt.
4. Click **Keep** again and complete the new Windows Hello prompt. Expect
   `PASS: Windows Hello assertion verified for Keep project:review-spike revision …`.
   Switch fixtures and repeat with **Discard**. Verify Windows requests a
   separate user action for each decision. Report cached or absent prompts
   as a failed per-action presence check.
5. Close the experiment. Its directory contains `receipt.txt` with local
   result lines and synthetic digests. The app holds the enrolled public key
   in memory. Windows may retain the test passkey. The operator can remove it
   from Windows passkey settings if it appears there.

## Decision binding and limits

The app renders the exact text whose UTF-8 SHA-256 digest it binds. When an
operator selects an action, it starts a 60-second, single-use challenge over a
random nonce, preview revision, scope, and action. The verifier consumes the
challenge on its first submission, compares the current fixture, checks the
credential ID, RP ID hash, user-presence and user-verification flags, and
verifies the ES256 signature over authenticator data and the expected client
data hash with the enrolled key. The app returns a receipt for synthetic data.
Restart drops enrollment and pending challenges, so a prior assertion cannot
be reused with a new instance.

The trusted preview is this app's read-only fixture display. Windows Hello
authenticates the operator and signs the challenge. It does not display the
note or attest to its meaning. A production implementation still needs
owner-controlled enrollment, a stable app identity, an independent Go
authority, and protection for the installed public key. The smoke has no
trusted display beyond this process and no persistent authority. It does not
qualify Windows, WSL transport, the real desktop client, or the release.

The spike was built with the native x64 compiler and passed all 17 software checks. WebAuthn
reported API v9 and a platform verifier in SSH Session 0. Session 0 cannot
show the interactive desktop. The actual enrollment, cancellation, and per-
decision Windows Hello prompts remain open.

[Microsoft WebAuthn API](https://learn.microsoft.com/en-us/windows/win32/api/webauthn/)
