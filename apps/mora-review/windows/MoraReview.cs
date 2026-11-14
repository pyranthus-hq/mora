using System;
using System.Collections;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using System.Web.Script.Serialization;
using System.Windows.Forms;

internal sealed class ReviewRow
{
    public string id { get; set; }
    public string kind { get; set; }
    public string scope { get; set; }
    public string type { get; set; }
    public string title { get; set; }
    public override string ToString() { return title + " — " + kind + " · " + scope + " · " + type; }
}

internal sealed class ReviewListReceipt { public string schema { get; set; } public int schema_version { get; set; } public List<ReviewRow> notes { get; set; } }
internal sealed class ReviewPreviewReceipt { public string schema { get; set; } public int schema_version { get; set; } public Dictionary<string, object> preview { get; set; } }

internal static class Program
{
    private const string ListSchema = "mora.review";
    private const string PreviewSchema = "mora.review.preview";
    private static readonly JavaScriptSerializer Json = new JavaScriptSerializer { MaxJsonLength = ReviewTransport.MaxBytes };

    [STAThread]
    private static int Main(string[] args)
    {
        try
        {
            if (args.Length == 2 && args[0] == "--test-stub") return Stub(args[1]);
            if (args.Length == 1 && args[0] == "--self-test") return SelfTest();
            if (args.Length == 1 && args[0] == "--stdio")
            {
                Application.EnableVisualStyles(); Application.SetCompatibleTextRenderingDefault(false);
                using (var channel = new DesktopChannel(Console.OpenStandardInput(), Console.OpenStandardOutput()))
                    Application.Run(new ReviewForm(null, channel));
                return 0;
            }
            string fixture = args.Length == 2 && args[0] == "--fixture" ? args[1] : null;
            if (args.Length != 0 && fixture == null) throw new ArgumentException("usage: MoraReview.exe [--self-test | --fixture path-to-canonical-preview.json]");
            Application.EnableVisualStyles(); Application.SetCompatibleTextRenderingDefault(false);
            Application.Run(new ReviewForm(fixture));
            return 0;
        }
        catch (Exception error) { Console.Error.WriteLine(error.Message); return 2; }
    }

    internal static List<ReviewRow> ParseList(string raw)
    {
        var receipt = Json.Deserialize<ReviewListReceipt>(raw);
        if (receipt == null || receipt.schema != ListSchema || receipt.schema_version != 1 || receipt.notes == null)
            throw new InvalidDataException("Mora returned an unsupported review queue.");
        foreach (var row in receipt.notes) if (!IsSafeRow(row)) throw new InvalidDataException("Mora returned an invalid review item.");
        return receipt.notes;
    }

    internal static Dictionary<string, object> ParsePreview(string raw, ReviewRow row)
    {
        var receipt = Json.Deserialize<ReviewPreviewReceipt>(raw);
        if (receipt == null || receipt.schema != PreviewSchema || receipt.schema_version != 1 || receipt.preview == null || !IsSafeRow(row))
            throw new InvalidDataException("Mora returned an unsupported review preview.");
        foreach (string key in new[] { "id", "kind", "title", "text", "scope", "type", "source", "provenance", "state", "complete", "redacted", "truncated", "escaped", "omitted_arguments", "canonical_revision", "display_digest" }) if (!receipt.preview.ContainsKey(key))
            throw new InvalidDataException("Mora returned an incomplete review preview.");
        if ((string)receipt.preview["id"] != row.id || (string)receipt.preview["kind"] != row.kind)
            throw new InvalidDataException("Mora returned a mismatched preview.");
        return receipt.preview;
    }

    internal static ReviewRow FixtureRow(string raw)
    {
        var receipt = Json.Deserialize<ReviewPreviewReceipt>(raw);
        if (receipt == null || receipt.schema != PreviewSchema || receipt.schema_version != 1 || receipt.preview == null)
            throw new InvalidDataException("The bundled review fixture is invalid.");
        var row = new ReviewRow { id = receipt.preview["id"] as string, kind = receipt.preview["kind"] as string, scope = receipt.preview["scope"] as string, type = receipt.preview["type"] as string, title = receipt.preview["title"] as string };
        if (!IsSafeRow(row)) throw new InvalidDataException("The bundled review fixture has an invalid review item.");
        return row;
    }

    internal static bool IsSafeRow(ReviewRow row)
    {
        if (row == null || (row.kind != "saved" && row.kind != "held") || String.IsNullOrEmpty(row.id) || row.id[0] == '-') return false;
        foreach (char c in row.id) if (!(Char.IsLetterOrDigit(c) || c == '_' || c == '.' || c == '-')) return false;
        return true;
    }

    internal static bool KeepEligible(Dictionary<string, object> item)
    {
        return (bool)item["complete"] && !(bool)item["redacted"] && !(bool)item["truncated"] && !(bool)item["escaped"] && !(bool)item["omitted_arguments"];
    }

    private static int SelfTest()
    {
        var rows = ParseList("{\"schema\":\"mora.review\",\"schema_version\":1,\"notes\":[{\"id\":\"mem_1\",\"kind\":\"saved\",\"scope\":\"global\",\"type\":\"fact\",\"title\":\"Fixture\"}]}");
        Check("queue", rows.Count == 1 && rows[0].id == "mem_1");
        Check("real empty queue", ParseList("{\"schema\":\"mora.review\",\"schema_version\":1,\"notes\":[]}").Count == 0);
        ExpectInvalid("null queue", delegate { ParseList("{\"schema\":\"mora.review\",\"schema_version\":1,\"notes\":null}"); });
        ExpectInvalid("argument row", delegate { ParseList("{\"schema\":\"mora.review\",\"schema_version\":1,\"notes\":[{\"id\":\"--help\",\"kind\":\"saved\",\"scope\":\"g\",\"type\":\"f\",\"title\":\"x\"}]}"); });
        var preview = ParsePreview("{\"schema\":\"mora.review.preview\",\"schema_version\":1,\"preview\":{\"id\":\"mem_1\",\"kind\":\"saved\",\"title\":\"Fixture\",\"text\":\"one\\ntwo Ω\",\"scope\":\"global\",\"type\":\"fact\",\"source\":\"mcp\",\"provenance\":\"authored\",\"state\":\"tentative\",\"complete\":true,\"redacted\":true,\"truncated\":false,\"escaped\":false,\"omitted_arguments\":false,\"canonical_revision\":\"r\",\"display_digest\":\"d\"}}", rows[0]);
        Check("optional details", !preview.ContainsKey("details")); Check("flag blocks keep", !KeepEligible(preview));
        Check("native line endings", ReviewForm.NativeLines((string)preview["text"]) == "one\r\ntwo Ω");
        preview["title"] = "Title\nsecond line";
        Check("all preview fields use native lines", !ReviewForm.Render(preview).Replace("\r\n", "").Contains("\n"));
        Check("quote plain", ReviewTransport.QuoteArgument("plain") == "\"plain\"");
        Check("quote trailing slash", ReviewTransport.QuoteArgument("C:\\path\\") == "\"C:\\path\\\\\"");
        Check("quote quote", ReviewTransport.QuoteArgument("a\"b") == "\"a\\\"b\"");
        using (var input = DesktopResponse("{\"schema\":\"mora.review.desktop\",\"schema_version\":1,\"enrolled\":true}"))
        using (var output = new MemoryStream())
        using (var channel = new DesktopChannel(input, output))
        {
            Check("desktop frame", Bool(channel.Request(channel.Frame("status")), "enrolled") && output.Length > 4);
        }
        ExpectInvalid("desktop frame error", delegate {
            using (var input = DesktopResponse("{\"schema\":\"mora.review.desktop\",\"schema_version\":1,\"error\":{\"code\":\"ERR_REVIEW_UNAVAILABLE\"}}"))
            using (var output = new MemoryStream())
            using (var channel = new DesktopChannel(input, output)) channel.Request(channel.Frame("status"));
        });
        Check("action status", ReviewForm.Outcome(new Dictionary<string, object> { { "outcome", new Dictionary<string, object> { { "status", "stale" }, { "index_visibility", "not_checked" } } } }).Contains("stale"));
        var flow = new ReviewFlow(); flow.BeginQueue(); var firstPreview = flow.BeginPreview();
        Check("single item preview is current", flow.IsCurrent(firstPreview));
        flow.BeginQueue(); Check("refresh drops a late preview", !flow.IsCurrent(firstPreview));
        string self = Process.GetCurrentProcess().MainModule.FileName;
        Check("concurrent drain", ReviewTransport.RunAsync(self, new[] { "--test-stub", "flood" }, 2000, ReviewTransport.MaxBytes, CancellationToken.None).GetAwaiter().GetResult() == "ok");
        ExpectInvalid("bounded stdout", delegate { ReviewTransport.RunAsync(self, new[] { "--test-stub", "oversize" }, 2000, 1024, CancellationToken.None).GetAwaiter().GetResult(); });
        try { ReviewTransport.RunAsync(self, new[] { "--test-stub", "hang" }, 50, ReviewTransport.MaxBytes, CancellationToken.None).GetAwaiter().GetResult(); throw new InvalidOperationException("timeout accepted"); }
        catch (TimeoutException) { }
        Console.WriteLine("PASS: review parser, selection input, quoting, CRLF rendering, bounded concurrent transport, and timeout.");
        return 0;
    }

    private static int Stub(string mode)
    {
        if (mode == "flood") { Console.Error.Write(new string('e', 128 * 1024)); Console.Out.Write("ok"); return 0; }
        if (mode == "oversize") { Console.Out.Write(new string('o', ReviewTransport.MaxBytes + 1)); return 0; }
        if (mode == "hang") { Thread.Sleep(1000); return 0; }
        return 2;
    }

    private static void Check(string label, bool condition) { if (!condition) throw new InvalidOperationException("FAIL: " + label); }
    private static void ExpectInvalid(string label, Action action) { try { action(); throw new InvalidOperationException("FAIL: " + label); } catch (InvalidDataException) { } }
    private static MemoryStream DesktopResponse(string body) { byte[] raw = new UTF8Encoding(false).GetBytes(body); var stream = new MemoryStream(); stream.WriteByte((byte)(raw.Length >> 24)); stream.WriteByte((byte)(raw.Length >> 16)); stream.WriteByte((byte)(raw.Length >> 8)); stream.WriteByte((byte)raw.Length); stream.Write(raw, 0, raw.Length); stream.Position = 0; return stream; }
}

internal static class ReviewTransport
{
    internal const int MaxBytes = 1048576;

    internal static async Task<string> RunAsync(string executable, string[] arguments, int timeoutMs, int limit, CancellationToken cancellation)
    {
        var start = new ProcessStartInfo { FileName = executable, Arguments = JoinArguments(arguments), UseShellExecute = false, RedirectStandardOutput = true, RedirectStandardError = true, CreateNoWindow = true };
        using (var process = Process.Start(start))
        using (var cancel = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
        {
            var stdout = ReadLimitedAsync(process.StandardOutput.BaseStream, limit, cancel.Token);
            var stderr = ReadLimitedAsync(process.StandardError.BaseStream, limit, cancel.Token);
            var stdoutWatch = stdout.ContinueWith(delegate(Task<byte[]> result) { if (result.IsFaulted) TryKill(process); });
            var stderrWatch = stderr.ContinueWith(delegate(Task<byte[]> result) { if (result.IsFaulted) TryKill(process); });
            var streams = Task.WhenAll(stdout, stderr);
            var exited = Task.Run(delegate { process.WaitForExit(); });
            var completed = Task.WhenAll(streams, exited, stdoutWatch, stderrWatch);
            var timer = Task.Delay(timeoutMs, cancellation);
            if (await Task.WhenAny(completed, timer) != completed)
            {
                TryKill(process); cancel.Cancel();
                if (cancellation.IsCancellationRequested) throw new OperationCanceledException(cancellation);
                throw new TimeoutException("Mora did not respond within " + timeoutMs + " ms.");
            }
            try { await completed; }
            catch { TryKill(process); throw; }
            string output = Decode(stdout.Result), error = Decode(stderr.Result);
            if (process.ExitCode != 0) throw new InvalidOperationException(String.IsNullOrEmpty(error) ? "Mora exited with status " + process.ExitCode + "." : error.Trim());
            return output;
        }
    }

    private static async Task<byte[]> ReadLimitedAsync(Stream stream, int limit, CancellationToken cancellation)
    {
        var buffer = new byte[8192]; var output = new MemoryStream();
        try {
            while (true) {
                int count = await stream.ReadAsync(buffer, 0, buffer.Length, cancellation);
                if (count == 0) return output.ToArray();
                if (output.Length + count > limit) throw new InvalidDataException("Mora returned more than " + limit + " bytes.");
                output.Write(buffer, 0, count);
            }
        } finally { output.Dispose(); }
    }

    private static string Decode(byte[] bytes) { return new UTF8Encoding(false, true).GetString(bytes); }
    private static void TryKill(Process process) { try { if (!process.HasExited) process.Kill(); } catch (InvalidOperationException) { } }

    internal static string QuoteArgument(string argument)
    {
        if (argument == null || argument.IndexOfAny(new[] { '\r', '\n', '\0' }) >= 0) throw new ArgumentException("Invalid Mora argument.");
        var result = new StringBuilder("\""); int slashes = 0;
        foreach (char c in argument) {
            if (c == '\\') { slashes++; continue; }
            if (c == '\"') result.Append('\\', slashes * 2 + 1).Append(c);
            else result.Append('\\', slashes).Append(c);
            slashes = 0;
        }
        result.Append('\\', slashes * 2).Append('\"');
        return result.ToString();
    }

    private static string JoinArguments(string[] arguments)
    {
        var result = new StringBuilder();
        foreach (string argument in arguments) { if (result.Length != 0) result.Append(' '); result.Append(QuoteArgument(argument)); }
        return result.ToString();
    }
}

internal sealed class ReviewToken { internal int Queue; internal int Selection; }
internal sealed class ReviewFlow
{
    private int queue, selection;
    internal int BeginQueue() { queue++; selection++; return queue; }
    internal ReviewToken BeginPreview() { selection++; return new ReviewToken { Queue = queue, Selection = selection }; }
    internal bool IsQueueCurrent(int value) { return value == queue; }
    internal bool IsCurrent(ReviewToken value) { return value.Queue == queue && value.Selection == selection; }
}

internal sealed class ReviewForm : Form
{
    private readonly ListBox queue = new ListBox();
    private readonly TextBox preview = new TextBox();
    private readonly Label status = new Label();
    private readonly Button refresh = new Button { Text = "Refresh", AutoSize = true };
    private readonly Button enroll = new Button { Text = "Enroll Windows Hello", AutoSize = true, Enabled = false };
    private readonly Button keep = new Button { Text = "Keep", Enabled = false, Visible = false };
    private readonly Button discard = new Button { Text = "Discard", Enabled = false, Visible = false };
    private readonly Button cancel = new Button { Text = "Cancel", Enabled = false, Visible = false };
    private readonly Button close = new Button { Text = "Close", AutoSize = true, DialogResult = DialogResult.Cancel };
    private readonly string fixture;
    private readonly CancellationTokenSource lifetime = new CancellationTokenSource();
    private List<ReviewRow> rows = new List<ReviewRow>();
    private readonly ReviewFlow flow = new ReviewFlow();
    private bool binding;
    private bool enrolled, busy;
    private string pendingNonce;
    private Dictionary<string, object> current;

    private readonly DesktopChannel channel;

    internal ReviewForm(string fixturePath) : this(fixturePath, null) { }
    internal ReviewForm(string fixturePath, DesktopChannel desktopChannel)
    {
        channel = desktopChannel;
        fixture = fixturePath; Text = fixture == null ? "Mora Review" : "Mora Review — synthetic fixture";
        MinimumSize = new System.Drawing.Size(400, 360); Size = new System.Drawing.Size(520, 560); StartPosition = FormStartPosition.CenterScreen;
        KeyPreview = true; KeyDown += delegate(object sender, KeyEventArgs e) { if (e.KeyCode == Keys.Escape) Close(); };
        FormClosing += delegate { lifetime.Cancel(); };
        status.AutoSize = false; status.Dock = DockStyle.Fill; status.TextAlign = System.Drawing.ContentAlignment.MiddleLeft; status.AccessibleName = "Review status";
        refresh.AccessibleName = "Refresh review queue"; queue.AccessibleName = "Agent notes waiting for review"; preview.AccessibleName = "Full bounded review preview";
        enroll.AccessibleName = "Enroll Windows Hello"; keep.AccessibleName = "Keep reviewed note"; discard.AccessibleName = "Discard reviewed note"; cancel.AccessibleName = "Cancel pending review decision"; close.AccessibleName = "Close review window";
        refresh.Click += async delegate { await LoadQueueAsync(); }; queue.SelectedIndexChanged += async delegate { if (!binding) await ShowSelectedAsync(queue.SelectedItem as ReviewRow); };
        enroll.Click += async delegate { await EnrollAsync(); };
        keep.Click += async delegate { await DecideAsync("keep"); };
        discard.Click += async delegate { await DecideAsync("discard"); };
        cancel.Click += async delegate { await CancelAsync("Decision cancelled. Nothing changed."); };
        var top = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, AutoSize = true };
        top.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100)); top.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize)); top.Controls.Add(status, 0, 0); top.Controls.Add(refresh, 1, 0);
        var actions = new FlowLayoutPanel { Dock = DockStyle.Fill, AutoSize = true, FlowDirection = FlowDirection.LeftToRight }; actions.Controls.AddRange(new Control[] { enroll, keep, discard, cancel, close });
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Padding = new Padding(12), RowCount = 4, ColumnCount = 1 };
        layout.RowStyles.Add(new RowStyle(SizeType.AutoSize)); layout.RowStyles.Add(new RowStyle(SizeType.Percent, 30)); layout.RowStyles.Add(new RowStyle(SizeType.Percent, 70)); layout.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        queue.Dock = DockStyle.Fill; preview.Dock = DockStyle.Fill; preview.Multiline = true; preview.ReadOnly = true; preview.ScrollBars = ScrollBars.Vertical;
        layout.Controls.Add(top, 0, 0); layout.Controls.Add(queue, 0, 1); layout.Controls.Add(preview, 0, 2); layout.Controls.Add(actions, 0, 3); Controls.Add(layout);
        CancelButton = close; refresh.TabIndex = 0; queue.TabIndex = 1; preview.TabIndex = 2; close.TabIndex = 3;
        Shown += async delegate { await LoadStatusAsync(); await LoadQueueAsync(); };
    }

    private async Task LoadQueueAsync()
    {
        int generation = flow.BeginQueue(); binding = true; refresh.Enabled = false; preview.Text = ""; current = null; SetActions(); queue.DataSource = null; status.Text = "Loading the saved and held queues…";
        try {
            if (fixture == null) rows = Program.ParseList(await RunMoraAsync(new[] { "review", "--list", "--json" }));
            else { string raw = await ReadFixtureAsync(); rows = new List<ReviewRow> { Program.FixtureRow(raw) }; }
            if (IsDisposed || lifetime.IsCancellationRequested || !flow.IsQueueCurrent(generation)) return;
            queue.DataSource = rows; binding = false;
            if (rows.Count == 0) { status.Text = "No agent notes wait for review."; return; }
            queue.SelectedIndex = 0; await ShowSelectedAsync(rows[0]);
        }
        catch (Exception error) { if (!IsDisposed && !lifetime.IsCancellationRequested && flow.IsQueueCurrent(generation)) { rows = new List<ReviewRow>(); queue.DataSource = null; preview.Text = NativeLines(error.Message); current = null; SetActions(); status.Text = "Review unavailable. See details below."; } }
        finally { if (!IsDisposed && !lifetime.IsCancellationRequested && flow.IsQueueCurrent(generation)) { binding = false; refresh.Enabled = true; } }
    }

    private async Task ShowSelectedAsync(ReviewRow row)
    {
        if (!Program.IsSafeRow(row)) { status.Text = "Preview failed: Mora returned an invalid review item."; return; }
        ReviewToken token = flow.BeginPreview(); preview.Text = ""; current = null; SetActions(); status.Text = "Loading full bounded preview…";
        try {
            Dictionary<string, object> shown = fixture == null ? Program.ParsePreview(await RunMoraAsync(new[] { "review", "--show", row.kind, row.id, "--json" }), row) : Program.ParsePreview(await ReadFixtureAsync(), row);
            if (IsDisposed || lifetime.IsCancellationRequested || !flow.IsCurrent(token)) return;
            current = shown; preview.Text = Render(shown); SetActions(); status.Text = channel == null ? "Read-only preview. Launch through Mora to approve changes." : enrolled ? "Review the canonical preview, then choose an action." : "Read-only until Windows Hello enrollment completes.";
        }
        catch (Exception error) { if (!IsDisposed && !lifetime.IsCancellationRequested && flow.IsCurrent(token)) { current = null; SetActions(); preview.Text = NativeLines(error.Message); status.Text = "Preview failed. See details below."; } }
    }

    private async Task LoadStatusAsync()
    {
        if (channel == null) { status.Text = fixture == null ? "Read-only developer shell. Start Mora Review from its shortcut to use the Go-owned review route." : "Synthetic fixture: read-only."; return; }
        try {
            var response = await ChannelAsync(channel.Frame("status"));
            enrolled = Bool(response, "enrolled");
            if (!WindowsHello.Available()) status.Text = "Read-only. Windows Hello platform verification is unavailable.";
            else status.Text = enrolled ? "Windows Hello enrolled. Loading review queue…" : "Read-only until Windows Hello enrollment completes.";
            SetActions();
        } catch (Exception error) { status.Text = "Read-only. " + error.Message; SetActions(); }
    }

    private async Task EnrollAsync()
    {
        if (channel == null || busy || !WindowsHello.Available()) return;
        SetBusy(true, "Requesting a one-time enrollment challenge…");
        string nonce = null;
        try {
            var begin = await ChannelAsync(channel.Frame("enroll.begin"));
            nonce = RequiredString(begin, "nonce");
            var enrollment = WindowsHello.Enroll(Handle, Decode(RequiredString(begin, "challenge_bytes_b64")));
            var complete = channel.Frame("enroll.complete"); complete["nonce"] = nonce; AddProof(complete, enrollment);
            var result = await ChannelAsync(complete);
            enrolled = Bool(result, "enrolled");
            if (!enrolled) throw new InvalidDataException("Windows Hello enrollment was not accepted.");
            status.Text = "Windows Hello enrolled. Review the canonical preview before applying a decision.";
        } catch (Exception error) { await CancelIfPendingAsync(nonce); status.Text = "Read-only. Enrollment was not completed. " + error.Message; }
        finally { SetBusy(false, null); }
    }

    private async Task DecideAsync(string action)
    {
        if (channel == null || !enrolled || busy || current == null) return;
        if (action == "discard" && MessageBox.Show(this, "Discard this note? This cannot be undone.", "Confirm discard", MessageBoxButtons.OKCancel, MessageBoxIcon.Warning) != DialogResult.OK) return;
        SetBusy(true, "Binding the displayed preview to a fresh Windows Hello prompt…");
        try {
            var begin = channel.Frame("decision.begin"); begin["action"] = action; begin["kind"] = current["kind"]; begin["id"] = current["id"]; begin["canonical_revision"] = current["canonical_revision"]; begin["display_digest"] = current["display_digest"];
            var started = await ChannelAsync(begin); pendingNonce = RequiredString(started, "nonce");
            var assertion = WindowsHello.Assert(Handle, Decode(RequiredString(started, "challenge_bytes_b64")));
            var complete = channel.Frame("decision.complete"); complete["nonce"] = pendingNonce; AddProof(complete, assertion);
            var response = await ChannelAsync(complete); pendingNonce = null;
            status.Text = Outcome(response); await LoadQueueAsync();
        } catch (Exception error) { await CancelIfPendingAsync(pendingNonce); status.Text = "No decision was applied. " + error.Message; }
        finally { SetBusy(false, null); }
    }

    private async Task CancelAsync(string message) { if (busy || pendingNonce == null) return; await CancelIfPendingAsync(pendingNonce); status.Text = message; SetActions(); }
    private async Task CancelIfPendingAsync(string nonce) { if (channel == null || String.IsNullOrEmpty(nonce)) return; try { var request = channel.Frame("cancel"); request["nonce"] = nonce; await ChannelAsync(request); } catch (Exception) { } finally { if (pendingNonce == nonce) pendingNonce = null; } }
    private Task<Dictionary<string, object>> ChannelAsync(Dictionary<string, object> request) { return Task.Run(delegate { return channel.Request(request); }); }
    private static string RequiredString(Dictionary<string, object> data, string key) { object value; if (!data.TryGetValue(key, out value) || !(value is string) || String.IsNullOrEmpty((string)value)) throw new InvalidDataException("Mora returned an incomplete desktop response."); return (string)value; }
    private static bool Bool(Dictionary<string, object> data, string key) { object value; return data.TryGetValue(key, out value) && value is bool && (bool)value; }
    private static byte[] Decode(string value) { try { return Convert.FromBase64String(value); } catch (FormatException) { throw new InvalidDataException("Mora returned invalid desktop proof bytes."); } }
    private static void AddProof(Dictionary<string, object> target, WindowsHello.Proof proof) { target["credential_id"] = Convert.ToBase64String(proof.CredentialID); target["client_data_json_b64"] = Convert.ToBase64String(proof.ClientDataJSON); target["authenticator_data_b64"] = Convert.ToBase64String(proof.AuthenticatorData); target["assertion_signature_der_b64"] = Convert.ToBase64String(proof.SignatureDER); }
    internal static string Outcome(Dictionary<string, object> response) { object raw; if (!response.TryGetValue("outcome", out raw)) throw new InvalidDataException("Mora returned no decision outcome."); var outcome = raw as Dictionary<string, object>; if (outcome == null) throw new InvalidDataException("Mora returned an invalid decision outcome."); object status; object lag; if (!outcome.TryGetValue("status", out status) || !(status is string) || !outcome.TryGetValue("index_visibility", out lag) || !(lag is string)) throw new InvalidDataException("Mora returned an incomplete decision outcome."); string state = (string)status; if (state != "applied" && state != "cancelled" && state != "stale") state = "unknown"; return "Decision " + state + ". Index visibility: " + lag + ". Refreshing queue…"; }
    private void SetBusy(bool value, string text) { busy = value; refresh.Enabled = !value; queue.Enabled = !value; if (text != null) status.Text = text; SetActions(); }
    private void SetActions() { bool trusted = channel != null && !busy; enroll.Visible = channel != null; enroll.Enabled = trusted && !enrolled; bool shown = trusted && enrolled && current != null; keep.Visible = shown && Program.KeepEligible(current); keep.Enabled = keep.Visible; discard.Visible = shown; discard.Enabled = shown; cancel.Visible = channel != null && pendingNonce != null; cancel.Enabled = cancel.Visible && !busy; }

    private async Task<string> RunMoraAsync(string[] arguments)
    {
        if (channel != null) return await Task.Run(delegate { return channel.Run(arguments); });
        string mora = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "mora.exe");
        if (!File.Exists(mora)) throw new FileNotFoundException("The installed Mora binary was not found beside Mora Review.", mora);
        return await ReviewTransport.RunAsync(mora, arguments, 30000, ReviewTransport.MaxBytes, lifetime.Token);
    }
    private async Task<string> ReadFixtureAsync() { return await Task.Run(delegate { return File.ReadAllText(fixture, Encoding.UTF8); }); }
    internal static string NativeLines(string value) { return value.Replace("\r\n", "\n").Replace("\r", "\n").Replace("\n", "\r\n"); }
    internal static string Render(Dictionary<string, object> item)
    {
        var text = new StringBuilder();
        foreach (string key in new[] { "title", "kind", "state", "scope", "type", "source", "provenance", "canonical_revision", "display_digest" }) text.Append(key.Replace('_', ' ')).Append(": ").Append(item[key]).AppendLine().AppendLine();
        text.Append("text: ").Append(NativeLines((string)item["text"])).AppendLine().AppendLine();
        object rawDetails; if (item.TryGetValue("details", out rawDetails) && rawDetails != null) foreach (object entry in (IEnumerable)rawDetails) { var detail = (Dictionary<string, object>)entry; text.Append(detail["label"]).Append(": ").Append(detail["value"]).AppendLine(); }
        var incomplete = new List<string>(); foreach (string key in new[] { "redacted", "truncated", "escaped", "omitted_arguments" }) if ((bool)item[key]) incomplete.Add(key.Replace('_', ' '));
        if (!Program.KeepEligible(item)) { if (incomplete.Count == 0) incomplete.Add("Mora marked this preview incomplete"); text.AppendLine().Append("INCOMPLETE PREVIEW: ").Append(string.Join(", ", incomplete.ToArray())).AppendLine(". Keep is unavailable."); }
        text.AppendLine().Append("Index visibility is not reported by this read-only preview. Search may lag; this app does not claim the note is indexed."); return NativeLines(text.ToString());
    }
}
