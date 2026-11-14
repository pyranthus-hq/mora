using System;
using System.Collections;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Text;
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

internal sealed class ReviewListReceipt
{
    public string schema { get; set; }
    public int schema_version { get; set; }
    public List<ReviewRow> notes { get; set; }
}

internal sealed class ReviewPreviewReceipt
{
    public string schema { get; set; }
    public int schema_version { get; set; }
    public Dictionary<string, object> preview { get; set; }
}

internal static class Program
{
    private const string ListSchema = "mora.review";
    private const string PreviewSchema = "mora.review.preview";
    private static readonly JavaScriptSerializer Json = new JavaScriptSerializer { MaxJsonLength = 1048576 };

    [STAThread]
    private static int Main(string[] args)
    {
        try
        {
            if (args.Length == 1 && args[0] == "--self-test") return SelfTest();
            string fixture = args.Length == 2 && args[0] == "--fixture" ? args[1] : null;
            if (args.Length != 0 && fixture == null) throw new ArgumentException("usage: MoraReview.exe [--self-test | --fixture path-to-canonical-preview.json]");
            Application.EnableVisualStyles();
            Application.SetCompatibleTextRenderingDefault(false);
            Application.Run(new ReviewForm(fixture));
            return 0;
        }
        catch (Exception error)
        {
            Console.Error.WriteLine(error.Message);
            return 2;
        }
    }

    internal static ReviewListReceipt ParseList(string raw)
    {
        var receipt = Json.Deserialize<ReviewListReceipt>(raw);
        if (receipt == null || receipt.schema != ListSchema || receipt.schema_version != 1 || receipt.notes == null)
            throw new InvalidDataException("Mora returned an unsupported review queue.");
        return receipt;
    }

    internal static ReviewPreviewReceipt ParsePreview(string raw)
    {
        var receipt = Json.Deserialize<ReviewPreviewReceipt>(raw);
        if (receipt == null || receipt.schema != PreviewSchema || receipt.schema_version != 1 || receipt.preview == null)
            throw new InvalidDataException("Mora returned an unsupported review preview.");
        return receipt;
    }

    private static int SelfTest()
    {
        var list = ParseList("{\"schema\":\"mora.review\",\"schema_version\":1,\"notes\":[{\"id\":\"mem_1\",\"kind\":\"saved\",\"scope\":\"global\",\"type\":\"fact\",\"title\":\"Fixture\"}]}");
        if (list.notes.Count != 1 || list.notes[0].id != "mem_1") throw new InvalidOperationException("queue fixture failed");
        var preview = ParsePreview("{\"schema\":\"mora.review.preview\",\"schema_version\":1,\"preview\":{\"id\":\"mem_1\",\"kind\":\"saved\",\"title\":\"Fixture\",\"text\":\"Text\",\"scope\":\"global\",\"type\":\"fact\",\"source\":\"mcp\",\"provenance\":\"authored\",\"state\":\"tentative\",\"details\":[],\"complete\":false,\"redacted\":true,\"truncated\":false,\"escaped\":false,\"omitted_arguments\":false,\"canonical_revision\":\"r\",\"display_digest\":\"d\"}}");
        if ((bool)preview.preview["complete"] || !(bool)preview.preview["redacted"]) throw new InvalidOperationException("incomplete preview fixture failed");
        try { ParseList("{\"schema\":\"wrong\",\"schema_version\":1,\"notes\":[]}"); throw new InvalidOperationException("wrong schema accepted"); }
        catch (InvalidDataException) { }
        Console.WriteLine("PASS: canonical queue, canonical preview, incomplete preview, and schema rejection.");
        return 0;
    }
}

internal sealed class ReviewForm : Form
{
    private readonly ListBox queue = new ListBox();
    private readonly TextBox preview = new TextBox();
    private readonly Label status = new Label();
    private readonly Button keep = new Button();
    private readonly Button discard = new Button();
    private readonly string fixture;
    private List<ReviewRow> rows = new List<ReviewRow>();

    internal ReviewForm(string fixturePath)
    {
        fixture = fixturePath;
        Text = fixture == null ? "Mora Review" : "Mora Review — synthetic fixture";
        MinimumSize = new System.Drawing.Size(400, 360);
        Size = new System.Drawing.Size(520, 560);
        StartPosition = FormStartPosition.CenterScreen;

        var refresh = new Button { Text = "Refresh", AutoSize = true, Anchor = AnchorStyles.Top | AnchorStyles.Right, Left = 420, Top = 12 };
        refresh.Click += delegate { LoadQueue(); };
        status.Text = "Reading the saved and held queues.";
        status.AutoSize = false; status.Left = 12; status.Top = 12; status.Width = 395; status.Height = 36;
        queue.Left = 12; queue.Top = 54; queue.Width = 480; queue.Height = 130; queue.Anchor = AnchorStyles.Top | AnchorStyles.Left | AnchorStyles.Right;
        queue.SelectedIndexChanged += delegate { ShowSelected(); };
        preview.Left = 12; preview.Top = 194; preview.Width = 480; preview.Height = 260; preview.Multiline = true; preview.ReadOnly = true; preview.ScrollBars = ScrollBars.Vertical;
        preview.Anchor = AnchorStyles.Top | AnchorStyles.Bottom | AnchorStyles.Left | AnchorStyles.Right;
        keep.Text = "Keep (unavailable)"; keep.Enabled = false; keep.Left = 12; keep.Top = 466; keep.Anchor = AnchorStyles.Bottom | AnchorStyles.Left;
        keep.Visible = false;
        discard.Text = "Discard (unavailable)"; discard.Enabled = false; discard.Left = 142; discard.Top = 466; discard.Anchor = AnchorStyles.Bottom | AnchorStyles.Left;
        Controls.AddRange(new Control[] { status, refresh, queue, preview, keep, discard });
        LoadQueue();
    }

    private void LoadQueue()
    {
        try
        {
            if (fixture != null)
            {
                var receipt = Program.ParsePreview(File.ReadAllText(fixture));
                rows = new List<ReviewRow> { RowFromPreview(receipt.preview) };
                status.Text = "SYNTHETIC FIXTURE — no Mora command ran and no note can change.";
            }
            else
            {
                var receipt = Program.ParseList(RunMora("review", "--list", "--json"));
                rows = receipt.notes;
                status.Text = rows.Count == 0 ? "No agent notes wait for review." : "Choose a note to see its full bounded preview.";
            }
            queue.DataSource = null; queue.DataSource = rows;
            preview.Text = "";
            keep.Visible = false;
        }
        catch (Exception error) { queue.DataSource = null; preview.Text = ""; status.Text = "Review unavailable: " + error.Message; }
    }

    private void ShowSelected()
    {
        var row = queue.SelectedItem as ReviewRow;
        if (row == null) return;
        try
        {
            Dictionary<string, object> shown = fixture == null ? Program.ParsePreview(RunMora("review", "--show", row.kind, row.id, "--json")).preview : Program.ParsePreview(File.ReadAllText(fixture)).preview;
            if ((string)shown["id"] != row.id || (string)shown["kind"] != row.kind) throw new InvalidDataException("Mora returned a mismatched preview.");
            keep.Visible = (bool)shown["complete"];
            preview.Text = Render(shown);
            status.Text = "Read-only preview. Desktop approvals are unavailable; use mora review in your own terminal to decide.";
        }
        catch (Exception error) { keep.Visible = false; preview.Text = ""; status.Text = "Preview failed: " + error.Message; }
    }

    private static ReviewRow RowFromPreview(Dictionary<string, object> item)
    {
        return new ReviewRow { id = (string)item["id"], kind = (string)item["kind"], scope = (string)item["scope"], type = (string)item["type"], title = (string)item["title"] };
    }

    private static string Render(Dictionary<string, object> item)
    {
        var text = new StringBuilder();
        foreach (string key in new[] { "title", "kind", "state", "scope", "type", "source", "provenance", "text", "canonical_revision", "display_digest" })
            text.Append(key.Replace('_', ' ')).Append(": ").Append(item[key]).AppendLine().AppendLine();
        object rawDetails;
        if (item.TryGetValue("details", out rawDetails)) foreach (object entry in (IEnumerable)rawDetails)
        {
            var detail = (Dictionary<string, object>)entry;
            text.Append(detail["label"]).Append(": ").Append(detail["value"]).AppendLine();
        }
        text.AppendLine();
        var incomplete = new List<string>();
        foreach (string key in new[] { "redacted", "truncated", "escaped", "omitted_arguments" }) if ((bool)item[key]) incomplete.Add(key.Replace('_', ' '));
        if (!(bool)item["complete"])
        {
            if (incomplete.Count == 0) incomplete.Add("Mora marked this preview incomplete");
            text.Append("INCOMPLETE PREVIEW: ").Append(string.Join(", ", incomplete.ToArray())).AppendLine(". Keep is unavailable.").AppendLine();
        }
        text.AppendLine("Index visibility is not reported by this read-only preview. Search may lag; this app does not claim the note is indexed.");
        return text.ToString();
    }

    private static string RunMora(params string[] arguments)
    {
        string mora = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "mora.exe");
        if (!File.Exists(mora)) throw new FileNotFoundException("The installed Mora binary was not found beside Mora Review.", mora);
        var start = new ProcessStartInfo { FileName = mora, Arguments = JoinArguments(arguments), UseShellExecute = false, RedirectStandardOutput = true, RedirectStandardError = true, CreateNoWindow = true };
        using (var process = Process.Start(start))
        {
            string output = process.StandardOutput.ReadToEnd(); string error = process.StandardError.ReadToEnd();
            process.WaitForExit();
            if (process.ExitCode != 0) throw new InvalidOperationException(string.IsNullOrEmpty(error) ? "Mora exited with status " + process.ExitCode + "." : error.Trim());
            if (output.Length > 1048576) throw new InvalidDataException("Mora returned more than 1 MiB.");
            return output;
        }
    }

    // .NET Framework has no ProcessStartInfo.ArgumentList; these are passed to the
    // executable with UseShellExecute=false, never through cmd.exe or PowerShell.
    private static string JoinArguments(string[] arguments)
    {
        var result = new StringBuilder();
        foreach (string argument in arguments)
        {
            if (argument == null || argument.IndexOfAny(new[] { '\r', '\n', '\0' }) >= 0) throw new ArgumentException("Invalid Mora argument.");
            if (result.Length != 0) result.Append(' ');
            result.Append('"').Append(argument.Replace("\\", "\\\\").Replace("\"", "\\\"")).Append('"');
        }
        return result.ToString();
    }
}
