using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;

// Minimal WebAuthn v1 binding. Go validates every returned credential field;
// this code only requests a platform, UV-required ES256 ceremony.
internal static class WindowsHello
{
    private const uint Platform = 1, VerificationRequired = 1, NoAttestation = 1;
    private const string PublicKey = "public-key", RpID = "mora-review.local";

    internal sealed class Proof
    {
        internal readonly byte[] CredentialID, ClientDataJSON, AuthenticatorData, SignatureDER;
        internal Proof(byte[] id, byte[] client, byte[] auth, byte[] signature) { CredentialID = id; ClientDataJSON = client; AuthenticatorData = auth; SignatureDER = signature; }
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] private struct Rp { public uint Version; public string Id, Name, Icon; }
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] private struct User { public uint Version, IdLength; public IntPtr Id; public string Name, Icon, DisplayName; }
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] private struct Client { public uint Version, Length; public IntPtr Json; public string HashAlgorithm; }
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] private struct Algorithm { public uint Version; public string Type; public int Id; }
    [StructLayout(LayoutKind.Sequential)] private struct NativeList { public uint Count; public IntPtr Items; }
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] private struct Credential { public uint Version, IdLength; public IntPtr Id; public string Type; }
    [StructLayout(LayoutKind.Sequential)] private struct MakeOptions { public uint Version, Timeout; public NativeList Excluded, Extensions; public uint Attachment, ResidentRequired, Verification, Attestation, Flags; }
    [StructLayout(LayoutKind.Sequential)] private struct GetOptions { public uint Version, Timeout; public NativeList Allowed, Extensions; public uint Attachment, Verification, Flags; }
    [StructLayout(LayoutKind.Sequential)] private struct MakeResult { public uint Version; public IntPtr Format; public uint AuthLength; public IntPtr Auth; public uint AttestationLength; public IntPtr Attestation; public uint AttestationType; public IntPtr Decoded; public uint ObjectLength; public IntPtr Object; public uint CredentialLength; public IntPtr Credential; }
    [StructLayout(LayoutKind.Sequential)] private struct GetResult { public uint Version, AuthLength; public IntPtr Auth; public uint SignatureLength; public IntPtr Signature; public Credential Credential; }

    [DllImport("webauthn.dll")] private static extern int WebAuthNIsUserVerifyingPlatformAuthenticatorAvailable(out int available);
    [DllImport("webauthn.dll")] private static extern int WebAuthNAuthenticatorMakeCredential(IntPtr window, ref Rp rp, ref User user, ref NativeList algorithms, ref Client client, ref MakeOptions options, out IntPtr result);
    [DllImport("webauthn.dll", CharSet = CharSet.Unicode, ExactSpelling = true)] private static extern int WebAuthNAuthenticatorGetAssertion(IntPtr window, string rp, ref Client client, ref GetOptions options, out IntPtr result);
    [DllImport("webauthn.dll")] private static extern void WebAuthNFreeCredentialAttestation(IntPtr result);
    [DllImport("webauthn.dll")] private static extern void WebAuthNFreeAssertion(IntPtr result);

    internal static bool Available()
    {
        try { int available; return WebAuthNIsUserVerifyingPlatformAuthenticatorAvailable(out available) == 0 && available != 0; }
        catch (DllNotFoundException) { return false; }
        catch (EntryPointNotFoundException) { return false; }
    }

    internal static Proof Enroll(IntPtr window, byte[] challenge)
    {
        using (var memory = new NativeMemory())
        {
            var rp = new Rp { Version = 1, Id = RpID, Name = "Mora Review" };
            byte[] userID = RandomBytes(32);
            var user = new User { Version = 1, IdLength = (uint)userID.Length, Id = memory.Pin(userID), Name = "mora-review", DisplayName = "Mora Review" };
            var algorithms = memory.List(new Algorithm { Version = 1, Type = PublicKey, Id = -7 });
            byte[] clientData = ClientData("webauthn.create", challenge);
            var client = CreateClient(clientData, memory);
            var options = new MakeOptions { Version = 1, Timeout = 60000, Attachment = Platform, Verification = VerificationRequired, Attestation = NoAttestation };
            IntPtr result;
            int hr = WebAuthNAuthenticatorMakeCredential(window, ref rp, ref user, ref algorithms, ref client, ref options, out result);
            try
            {
                CheckResult(hr, result);
                var data = (MakeResult)Marshal.PtrToStructure(result, typeof(MakeResult));
                return new Proof(Copy(data.Credential, data.CredentialLength, 1024), clientData, Copy(data.Auth, data.AuthLength, 4096), new byte[0]);
            }
            finally { if (result != IntPtr.Zero) WebAuthNFreeCredentialAttestation(result); }
        }
    }

    internal static Proof Assert(IntPtr window, byte[] challenge, byte[] credentialID)
    {
        if (credentialID == null || credentialID.Length == 0 || credentialID.Length > 1024) throw new InvalidDataException("Invalid enrolled credential.");
        using (var memory = new NativeMemory())
        {
            byte[] clientData = ClientData("webauthn.get", challenge);
            var client = CreateClient(clientData, memory);
            var credential = new Credential { Version = 1, IdLength = (uint)credentialID.Length, Id = memory.Pin(credentialID), Type = PublicKey };
            var options = new GetOptions { Version = 1, Timeout = 60000, Allowed = memory.List(credential), Attachment = Platform, Verification = VerificationRequired };
            IntPtr result;
            int hr = WebAuthNAuthenticatorGetAssertion(window, RpID, ref client, ref options, out result);
            try
            {
                CheckResult(hr, result);
                var data = (GetResult)Marshal.PtrToStructure(result, typeof(GetResult));
                return new Proof(Copy(data.Credential.Id, data.Credential.IdLength, 1024), clientData, Copy(data.Auth, data.AuthLength, 4096), Copy(data.Signature, data.SignatureLength, 72));
            }
            finally { if (result != IntPtr.Zero) WebAuthNFreeAssertion(result); }
        }
    }

    private static Client CreateClient(byte[] json, NativeMemory memory) { return new Client { Version = 1, Length = (uint)json.Length, Json = memory.Pin(json), HashAlgorithm = "SHA-256" }; }
    private static byte[] ClientData(string type, byte[] challenge) { return Encoding.UTF8.GetBytes("{\"type\":\"" + type + "\",\"challenge\":\"" + Base64URL(challenge) + "\",\"origin\":\"https://" + RpID + "\",\"crossOrigin\":false}"); }
    private static string Base64URL(byte[] bytes) { return Convert.ToBase64String(bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_'); }
    private static byte[] RandomBytes(int length) { var bytes = new byte[length]; using (var random = RandomNumberGenerator.Create()) random.GetBytes(bytes); return bytes; }
    private static void CheckResult(int hr, IntPtr result) { if (hr != 0 || result == IntPtr.Zero) throw new OperationCanceledException("Windows Hello did not provide proof (0x" + hr.ToString("X8") + ")."); }
    private static byte[] Copy(IntPtr source, uint length, int max) { if (source == IntPtr.Zero || length == 0 || length > max) throw new InvalidDataException("Unexpected Windows Hello buffer size."); var bytes = new byte[(int)length]; Marshal.Copy(source, bytes, 0, bytes.Length); return bytes; }

    private sealed class NativeMemory : IDisposable
    {
        private readonly List<GCHandle> pinned = new List<GCHandle>();
        private readonly List<Action> cleanup = new List<Action>();
        internal IntPtr Pin(byte[] bytes) { var handle = GCHandle.Alloc(bytes, GCHandleType.Pinned); pinned.Add(handle); return handle.AddrOfPinnedObject(); }
        internal NativeList List<T>(T value) where T : struct
        {
            IntPtr pointer = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(T)));
            try { Marshal.StructureToPtr(value, pointer, false); } catch { Marshal.FreeHGlobal(pointer); throw; }
            cleanup.Add(delegate { Marshal.DestroyStructure(pointer, typeof(T)); Marshal.FreeHGlobal(pointer); });
            return new NativeList { Count = 1, Items = pointer };
        }
        public void Dispose() { foreach (Action release in cleanup) release(); foreach (GCHandle handle in pinned) handle.Free(); }
    }
}
