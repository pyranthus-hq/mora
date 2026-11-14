using System;
using System.Collections.Generic;
using System.IO;
using System.Text;
using System.Web.Script.Serialization;

// The helper accepts only the Go-created inherited handles. There is no socket,
// listener, path argument, or fallback launch of mora.exe in this mode.
internal sealed class DesktopChannel : IDisposable
{
    private const int Limit = 1048576;
    private readonly Stream input, output;
    private readonly JavaScriptSerializer json = new JavaScriptSerializer { MaxJsonLength = Limit };

    internal DesktopChannel(Stream stdin, Stream stdout) { input = stdin; output = stdout; }

    internal string Run(string[] arguments)
    {
        Dictionary<string, object> request;
        if (arguments.Length == 3 && arguments[0] == "review" && arguments[1] == "--list" && arguments[2] == "--json") request = Frame("list");
        else if (arguments.Length == 5 && arguments[0] == "review" && arguments[1] == "--show" && arguments[4] == "--json") { request = Frame("show"); request["kind"] = arguments[2]; request["id"] = arguments[3]; }
        else throw new InvalidDataException("The desktop helper only supports canonical review list and preview requests.");
        Dictionary<string, object> response = Request(request);
        if ((string)request["op"] == "list") return json.Serialize(new Dictionary<string, object> { { "schema", "mora.review" }, { "schema_version", 1 }, { "notes", response["notes"] } });
        return json.Serialize(new Dictionary<string, object> { { "schema", "mora.review.preview" }, { "schema_version", 1 }, { "preview", response["preview"] } });
    }

    internal Dictionary<string, object> Frame(string op) { return new Dictionary<string, object> { { "schema", "mora.review.desktop" }, { "schema_version", 1 }, { "op", op } }; }
    internal Dictionary<string, object> Request(Dictionary<string, object> request)
    {
        Dictionary<string, object> response = Call(request);
        object error;
        if (response.TryGetValue("error", out error)) throw new InvalidDataException("Mora review channel is unavailable.");
        return response;
    }
    private Dictionary<string, object> Call(Dictionary<string, object> request)
    {
        byte[] body = new UTF8Encoding(false, true).GetBytes(json.Serialize(request));
        if (body.Length == 0 || body.Length > Limit) throw new InvalidDataException("Mora desktop frame exceeds the limit.");
        byte[] header = { (byte)(body.Length >> 24), (byte)(body.Length >> 16), (byte)(body.Length >> 8), (byte)body.Length };
        output.Write(header, 0, header.Length); output.Write(body, 0, body.Length); output.Flush();
        ReadExactly(header, 4); int size = (header[0] << 24) | (header[1] << 16) | (header[2] << 8) | header[3];
        if (size <= 0 || size > Limit) throw new InvalidDataException("Mora desktop response exceeds the limit.");
        body = new byte[size]; ReadExactly(body, size);
        var response = json.Deserialize<Dictionary<string, object>>(new UTF8Encoding(false, true).GetString(body));
        if (response == null || !response.ContainsKey("schema") || (string)response["schema"] != "mora.review.desktop" || !response.ContainsKey("schema_version") || Convert.ToInt32(response["schema_version"]) != 1) throw new InvalidDataException("Mora returned an unsupported desktop response.");
        return response;
    }
    private void ReadExactly(byte[] buffer, int count) { int offset = 0; while (offset < count) { int read = input.Read(buffer, offset, count - offset); if (read <= 0) throw new EndOfStreamException("Mora desktop channel closed."); offset += read; } }
    public void Dispose() { input.Dispose(); output.Dispose(); }
}
