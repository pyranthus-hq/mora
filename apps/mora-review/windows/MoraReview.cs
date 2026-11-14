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
        Check("quote plain", ReviewTransport.QuoteArgument("plain") == "\"plain\"");
        Check("quote trailing slash", ReviewTransport.QuoteArgument("C:\\path\\") == "\"C:\\path\\\\\"");
        Check("quote quote", ReviewTransport.QuoteArgument("a\"b") == "\"a\\\"b\"");
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
            stdout.ContinueWith(delegate(Task<byte[]> ignored) { TryKill(process); }, TaskContinuationOptions.OnlyOnFaulted);
            stderr.ContinueWith(delegate(Task<byte[]> ignored) { TryKill(process); }, TaskContinuationOptions.OnlyOnFaulted);
            var streams = Task.WhenAll(stdout, stderr);
            var exited = Task.Run(delegate { process.WaitForExit(); });
            var completed = Task.WhenAll(streams, exited);
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
    private readonly Button keep = new Button { Text = "Keep (unavailable)", Enabled = false, Visible = false };
    private readonly Button discard = new Button { Text = "Discard (unavailable)", Enabled = false };
    private readonly Button close = new Button { Text = "Close", AutoSize = true, DialogResult = DialogResult.Cancel };
    private readonly string fixture;
    private readonly CancellationTokenSource lifetime = new CancellationTokenSource();
    private List<ReviewRow> rows = new List<ReviewRow>();
    private readonly ReviewFlow flow = new ReviewFlow();
    private bool binding;

    internal ReviewForm(string fixturePath)
    {
        fixture = fixturePath; Text = fixture == null ? "Mora Review" : "Mora Review — synthetic fixture";
        MinimumSize = new System.Drawing.Size(400, 360); Size = new System.Drawing.Size(520, 560); StartPosition = FormStartPosition.CenterScreen;
        KeyPreview = true; KeyDown += delegate(object sender, KeyEventArgs e) { if (e.KeyCode == Keys.Escape) Close(); };
        FormClosing += delegate { lifetime.Cancel(); };
        status.AutoSize = false; status.Dock = DockStyle.Fill; status.TextAlign = System.Drawing.ContentAlignment.MiddleLeft; status.AccessibleName = "Review status";
        refresh.AccessibleName = "Refresh review queue"; queue.AccessibleName = "Agent notes waiting for review"; preview.AccessibleName = "Full bounded review preview";
        keep.AccessibleName = "Keep is unavailable"; discard.AccessibleName = "Discard is unavailable"; close.AccessibleName = "Close review window";
        refresh.Click += async delegate { await LoadQueueAsync(); }; queue.SelectedIndexChanged += async delegate { if (!binding) await ShowSelectedAsync(queue.SelectedItem as ReviewRow); };
        var top = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, AutoSize = true };
        top.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100)); top.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize)); top.Controls.Add(status, 0, 0); top.Controls.Add(refresh, 1, 0);
        var actions = new FlowLayoutPanel { Dock = DockStyle.Fill, AutoSize = true, FlowDirection = FlowDirection.LeftToRight }; actions.Controls.AddRange(new Control[] { keep, discard, close });
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Padding = new Padding(12), RowCount = 4, ColumnCount = 1 };
        layout.RowStyles.Add(new RowStyle(SizeType.AutoSize)); layout.RowStyles.Add(new RowStyle(SizeType.Percent, 30)); layout.RowStyles.Add(new RowStyle(SizeType.Percent, 70)); layout.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        queue.Dock = DockStyle.Fill; preview.Dock = DockStyle.Fill; preview.Multiline = true; preview.ReadOnly = true; preview.ScrollBars = ScrollBars.Vertical;
        layout.Controls.Add(top, 0, 0); layout.Controls.Add(queue, 0, 1); layout.Controls.Add(preview, 0, 2); layout.Controls.Add(actions, 0, 3); Controls.Add(layout);
        AcceptButton = refresh; CancelButton = close; refresh.TabIndex = 0; queue.TabIndex = 1; preview.TabIndex = 2; close.TabIndex = 3;
        Shown += async delegate { await LoadQueueAsync(); };
    }

    private async Task LoadQueueAsync()
    {
        int generation = flow.BeginQueue(); binding = true; refresh.Enabled = false; preview.Text = ""; keep.Visible = false; queue.DataSource = null; status.Text = "Loading the saved and held queues…";
        try {
            if (fixture == null) rows = Program.ParseList(await RunMoraAsync(new[] { "review", "--list", "--json" }));
            else { string raw = await ReadFixtureAsync(); rows = new List<ReviewRow> { Program.FixtureRow(raw) }; }
            if (!flow.IsQueueCurrent(generation)) return;
            queue.DataSource = rows; binding = false;
            if (rows.Count == 0) { status.Text = "No agent notes wait for review."; return; }
            queue.SelectedIndex = 0; await ShowSelectedAsync(rows[0]);
        }
        catch (Exception error) { if (flow.IsQueueCurrent(generation)) { rows = new List<ReviewRow>(); queue.DataSource = null; preview.Text = ""; keep.Visible = false; status.Text = "Review unavailable: " + error.Message; } }
        finally { if (flow.IsQueueCurrent(generation)) { binding = false; refresh.Enabled = true; } }
    }

    private async Task ShowSelectedAsync(ReviewRow row)
    {
        if (!Program.IsSafeRow(row)) { status.Text = "Preview failed: Mora returned an invalid review item."; return; }
        ReviewToken token = flow.BeginPreview(); preview.Text = ""; keep.Visible = false; status.Text = "Loading full bounded preview…";
        try {
            Dictionary<string, object> shown = fixture == null ? Program.ParsePreview(await RunMoraAsync(new[] { "review", "--show", row.kind, row.id, "--json" }), row) : Program.ParsePreview(await ReadFixtureAsync(), row);
            if (!flow.IsCurrent(token)) return;
            keep.Visible = Program.KeepEligible(shown); preview.Text = Render(shown); status.Text = "Read-only preview. Desktop approvals are unavailable because no trusted verifier is enrolled.";
        }
        catch (Exception error) { if (flow.IsCurrent(token)) { keep.Visible = false; preview.Text = ""; status.Text = "Preview failed: " + error.Message; } }
    }

    private async Task<string> RunMoraAsync(string[] arguments)
    {
        string mora = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "mora.exe");
        if (!File.Exists(mora)) throw new FileNotFoundException("The installed Mora binary was not found beside Mora Review.", mora);
        return await ReviewTransport.RunAsync(mora, arguments, 30000, ReviewTransport.MaxBytes, lifetime.Token);
    }
    private async Task<string> ReadFixtureAsync() { return await Task.Run(delegate { return File.ReadAllText(fixture, Encoding.UTF8); }); }
    internal static string NativeLines(string value) { return value.Replace("\r\n", "\n").Replace("\r", "\n").Replace("\n", "\r\n"); }
    private static string Render(Dictionary<string, object> item)
    {
        var text = new StringBuilder();
        foreach (string key in new[] { "title", "kind", "state", "scope", "type", "source", "provenance", "canonical_revision", "display_digest" }) text.Append(key.Replace('_', ' ')).Append(": ").Append(item[key]).AppendLine().AppendLine();
        text.Append("text: ").Append(NativeLines((string)item["text"])).AppendLine().AppendLine();
        object rawDetails; if (item.TryGetValue("details", out rawDetails) && rawDetails != null) foreach (object entry in (IEnumerable)rawDetails) { var detail = (Dictionary<string, object>)entry; text.Append(detail["label"]).Append(": ").Append(detail["value"]).AppendLine(); }
        var incomplete = new List<string>(); foreach (string key in new[] { "redacted", "truncated", "escaped", "omitted_arguments" }) if ((bool)item[key]) incomplete.Add(key.Replace('_', ' '));
        if (!Program.KeepEligible(item)) { if (incomplete.Count == 0) incomplete.Add("Mora marked this preview incomplete"); text.AppendLine().Append("INCOMPLETE PREVIEW: ").Append(string.Join(", ", incomplete.ToArray())).AppendLine(". Keep is unavailable."); }
        text.AppendLine().Append("Index visibility is not reported by this read-only preview. Search may lag; this app does not claim the note is indexed."); return text.ToString();
    }
}
