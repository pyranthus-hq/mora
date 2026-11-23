using System;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Security.Cryptography;
using System.Windows.Forms;

internal static class Spike
{
    private static readonly Preview Saved = new Preview("saved", "fixture-saved", "project:review-spike",
        "This synthetic note asks the agent to record that the sample project chose blue.");
    private static readonly Preview Held = new Preview("held", "fixture-held", "project:review-spike",
        "This synthetic proposal records that the sample project chose green.");

    [STAThread]
    private static int Main(string[] args)
    {
        try
        {
            if (args.Length == 1 && args[0] == "--self-test")
                return SelfTest();
            if (args.Length == 1 && args[0] == "--probe")
            {
                Console.WriteLine("WebAuthn API: {0}", WindowsHello.WebAuthNGetApiVersionNumber());
                Console.WriteLine("Process session: {0}", Process.GetCurrentProcess().SessionId);
                Console.WriteLine("Platform verifier available: {0}", WindowsHello.Available());
                return 0;
            }
            if (args.Length != 0)
            {
                Console.Error.WriteLine("Use --self-test, --probe, or launch with no arguments on the desktop.");
                return 2;
            }
            if (Process.GetCurrentProcess().SessionId == 0)
            {
                Console.Error.WriteLine("Read-only: launch in the interactive Windows desktop to use Windows Hello.");
                return 2;
            }
            Application.EnableVisualStyles();
            using (var window = new ReviewWindow())
                Application.Run(window);
            return 0;
        }
        catch (Exception err)
        {
            Console.Error.WriteLine(err.Message);
            return 1;
        }
    }

    private sealed class ReviewWindow : Form
    {
        private readonly TextBox body = new TextBox();
        private readonly Label status = new Label();
        private readonly Button enroll = new Button();
        private readonly Button keep = new Button();
        private readonly Button discard = new Button();
        private readonly Decision decision = new Decision();
        private Preview current = Saved;
        private Enrollment enrollment;

        internal ReviewWindow()
        {
            Text = "Mora approval spike (synthetic only)";
            ClientSize = new Size(760, 360);
            MinimumSize = new Size(680, 320);

            body.ReadOnly = true;
            body.Multiline = true;
            body.ScrollBars = ScrollBars.Vertical;
            body.Dock = DockStyle.Fill;
            body.Text = current.Display;

            status.Dock = DockStyle.Bottom;
            status.Height = 62;
            status.Padding = new Padding(8);

            var bar = new FlowLayoutPanel { Dock = DockStyle.Bottom, Height = 40, WrapContents = false };
            Configure(enroll, "Enroll fixture", Enroll);
            Configure(keep, "Keep", delegate { Review("Keep"); });
            Configure(discard, "Discard", delegate { Review("Discard"); });
            var next = new Button();
            Configure(next, "Switch fixture", delegate {
                decision.Cancel();
                current = current == Saved ? Held : Saved;
                body.Text = current.Display;
                SetStatus("Fixture changed. No decision recorded.");
            });
            var cancel = new Button();
            Configure(cancel, "Cancel", delegate {
                decision.Cancel();
                SetStatus("Cancelled. No decision recorded.");
            });
            bar.Controls.AddRange(new Control[] { enroll, keep, discard, next, cancel });

            Controls.Add(body);
            Controls.Add(bar);
            Controls.Add(status);
            keep.Enabled = discard.Enabled = false;
            try
            {
                enroll.Enabled = WindowsHello.Available();
            }
            catch (DllNotFoundException) { enroll.Enabled = false; }
            catch (EntryPointNotFoundException) { enroll.Enabled = false; }
            SetStatus(enroll.Enabled
                ? "Read-only until enrollment. Enroll a disposable test passkey with Windows Hello on this device."
                : "Read-only. Windows Hello platform verification is unavailable.");
        }

        private static void Configure(Button button, string name, Action click)
        {
            button.Text = name;
            button.AutoSize = true;
            button.Click += delegate { click(); };
        }

        private void Enroll()
        {
            try
            {
                enrollment = WindowsHello.Enroll(Handle);
                keep.Enabled = discard.Enabled = true;
                enroll.Enabled = false;
                SetStatus("Fixture enrolled. Review the note and select Keep, Discard, or Cancel.");
            }
            catch (Exception err)
            {
                enrollment = null;
                keep.Enabled = discard.Enabled = false;
                SetStatus("Read-only. No enrollment completed. " + err.Message);
            }
        }

        private void Review(string action)
        {
            try
            {
                if (enrollment == null || body.Text != current.Display)
                    throw new InvalidOperationException("The displayed fixture is unavailable.");
                byte[] client = decision.Begin(current, action, Proof.Clock.ElapsedMilliseconds);
                Assertion assertion = WindowsHello.Sign(Handle, enrollment, client);
                if (!decision.Commit(current, action, Proof.Clock.ElapsedMilliseconds, enrollment, assertion))
                {
                    SetStatus("Proof rejected. No decision recorded.");
                    return;
                }
                string revision = BitConverter.ToString(current.Revision).Replace("-", "").ToLowerInvariant();
                SetStatus("PASS: Windows Hello assertion verified for " + action + " " + current.Scope
                    + " revision " + revision);
            }
            catch (Exception err)
            {
                decision.Cancel();
                SetStatus("No decision recorded. " + err.Message);
            }
        }

        private void SetStatus(string message)
        {
            status.Text = message;
            string receipt = DateTime.UtcNow.ToString("o") + " " + message + Environment.NewLine;
            try
            {
                File.AppendAllText(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "receipt.txt"), receipt);
            }
            catch (IOException err) { status.Text += " Receipt file unavailable. " + err.Message; }
            catch (UnauthorizedAccessException) { status.Text += " Receipt file unavailable."; }
        }
    }

    private static int SelfTest()
    {
        using (var key = CngKey.Create(CngAlgorithm.ECDsaP256))
        using (var signer = new ECDsaCng(key))
        {
            byte[] credential = { 1, 2, 3 };
            byte[] auth = new byte[37];
            Buffer.BlockCopy(Proof.Hash(System.Text.Encoding.UTF8.GetBytes(Proof.Rp)), 0, auth, 0, 32);
            auth[32] = 0x05;
            byte[] pub = key.Export(CngKeyBlobFormat.EccPublicBlob);
            var enrollment = new Enrollment(credential, pub);
            var d = new Decision();
            const long now = 1200;
            Func<byte[], byte[], byte[], Assertion> sign = (client, id, data) =>
                new Assertion(id, data, EncodeDer(signer.SignHash(Proof.SigningHash(data, client))));

            byte[] begin = d.Begin(Saved, "Keep", now);
            Assertion proof = sign(begin, credential, auth);
            Check("saved synthetic proof", d.Commit(Saved, "Keep", now, enrollment, proof));
            Check("repeat rejected", !d.Commit(Saved, "Keep", now, enrollment, proof));
            Check("fresh challenge", !Proof.Equal(begin, d.Begin(Saved, "Keep", now)));
            Check("prior challenge rejected", !d.Commit(Saved, "Keep", now, enrollment, proof));

            begin = d.Begin(Held, "Discard", now);
            Check("held synthetic proof", d.Commit(Held, "Discard", now, enrollment, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            Check("missing proof rejected", !d.Commit(Saved, "Keep", now, enrollment, null));
            begin = d.Begin(Saved, "Keep", now);
            Check("unavailable enrollment rejected", !d.Commit(Saved, "Keep", now, null, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            var changed = new Preview("saved", "fixture-saved", Saved.Scope, "Changed synthetic body.");
            Check("changed preview rejected", !d.Commit(changed, "Keep", now, enrollment, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            var scoped = new Preview("saved", "fixture-saved", "project:other-spike", Saved.Display);
            Check("changed scope rejected", !d.Commit(scoped, "Keep", now, enrollment, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            Check("changed action rejected", !d.Commit(Saved, "Discard", now, enrollment, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            Check("expired proof rejected", !d.Commit(Saved, "Keep", now + 60000, enrollment, sign(begin, credential, auth)));
            begin = d.Begin(Saved, "Keep", now);
            d.Cancel();
            Check("cancel clears pending decision", !d.Commit(Saved, "Keep", now, enrollment, sign(begin, credential, auth)));

            begin = d.Begin(Saved, "Keep", now);
            Check("wrong credential rejected", !d.Commit(Saved, "Keep", now, enrollment, sign(begin, new byte[] { 9 }, auth)));
            begin = d.Begin(Saved, "Keep", now);
            var wrongRp = (byte[])auth.Clone();
            wrongRp[0] ^= 1;
            Check("wrong relying party rejected", !d.Commit(Saved, "Keep", now, enrollment, sign(begin, credential, wrongRp)));
            begin = d.Begin(Saved, "Keep", now);
            var noUv = (byte[])auth.Clone();
            noUv[32] = 0x01;
            Check("presence without verification rejected", !d.Commit(Saved, "Keep", now, enrollment, sign(begin, credential, noUv)));

            begin = d.Begin(Saved, "Keep", now);
            proof = sign(begin, credential, auth);
            proof.Signature[proof.Signature.Length - 1] ^= 1;
            Check("bad signature rejected", !d.Commit(Saved, "Keep", now, enrollment, proof));

            byte[] enrolledAuth;
            using (var stream = new MemoryStream())
            {
                stream.Write(auth, 0, auth.Length);
                stream.Write(new byte[16], 0, 16);
                stream.WriteByte(0);
                stream.WriteByte((byte)credential.Length);
                stream.Write(credential, 0, credential.Length);
                var keyPrefix = new byte[] { 0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20 };
                stream.Write(keyPrefix, 0, keyPrefix.Length);
                stream.Write(pub, 8, 32);
                stream.WriteByte(0x22);
                stream.WriteByte(0x58);
                stream.WriteByte(0x20);
                stream.Write(pub, 40, 32);
                enrolledAuth = stream.ToArray();
            }
            enrolledAuth[32] |= 0x40;
            Check("ES256 enrollment key parsed", Proof.Equal(pub, WindowsHello.ReadPublicKey(enrolledAuth, credential)));
        }
        Console.WriteLine("PASS: 17 synthetic checks. Windows Hello was not invoked.");
        return 0;
    }

    private static void Check(string label, bool result)
    {
        if (!result)
            throw new InvalidOperationException("FAIL: " + label);
        Console.WriteLine("PASS: " + label);
    }

    private static byte[] EncodeDer(byte[] raw)
    {
        if (raw.Length != 64)
            throw new InvalidDataException("Unexpected software signature width.");
        using (var stream = new MemoryStream())
        {
            stream.WriteByte(0x30);
            stream.WriteByte(0);
            for (int i = 0; i < 64; i += 32)
            {
                int end = i + 32;
                int start = i;
                while (start < end - 1 && raw[start] == 0)
                    start++;
                bool pad = raw[start] >= 128;
                stream.WriteByte(2);
                stream.WriteByte((byte)(end - start + (pad ? 1 : 0)));
                if (pad) stream.WriteByte(0);
                stream.Write(raw, start, end - start);
            }
            var result = stream.ToArray();
            result[1] = (byte)(result.Length - 2);
            return result;
        }
    }
}
