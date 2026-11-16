using System;
using System.IO;
using System.Runtime.InteropServices;

internal static class WindowsHello
{
    private const uint Platform = 1;
    private const uint VerificationRequired = 1;
    private const uint NoAttestation = 1;
    private const string PublicKey = "public-key";

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Rp
    {
        public uint Version;
        public string Id, Name, Icon;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct User
    {
        public uint Version, IdLength;
        public IntPtr Id;
        public string Name, Icon, DisplayName;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Client
    {
        public uint Version, Length;
        public IntPtr Json;
        public string HashAlgorithm;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Algorithm
    {
        public uint Version;
        public string Type;
        public int Id;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct NativeList
    {
        public uint Count;
        public IntPtr Items;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Credential
    {
        public uint Version, IdLength;
        public IntPtr Id;
        public string Type;
    }

    // Options structs use only the Win32 version-1 fields.
    [StructLayout(LayoutKind.Sequential)]
    private struct MakeOptions
    {
        public uint Version, Timeout;
        public NativeList Excluded, Extensions;
        public uint Attachment, ResidentRequired, Verification, Attestation, Flags;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct GetOptions
    {
        public uint Version, Timeout;
        public NativeList Allowed, Extensions;
        public uint Attachment, Verification, Flags;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct MakeResult
    {
        public uint Version;
        public IntPtr Format;
        public uint AuthLength;
        public IntPtr Auth;
        public uint AttestationLength;
        public IntPtr Attestation;
        public uint AttestationType;
        public IntPtr Decoded;
        public uint ObjectLength;
        public IntPtr Object;
        public uint CredentialLength;
        public IntPtr Credential;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct GetResult
    {
        public uint Version, AuthLength;
        public IntPtr Auth;
        public uint SignatureLength;
        public IntPtr Signature;
        public Credential Credential;
    }

    [DllImport("webauthn.dll")]
    internal static extern uint WebAuthNGetApiVersionNumber();

    [DllImport("webauthn.dll")]
    private static extern int WebAuthNIsUserVerifyingPlatformAuthenticatorAvailable(out int available);

    [DllImport("webauthn.dll")]
    private static extern int WebAuthNAuthenticatorMakeCredential(IntPtr window, ref Rp rp, ref User user,
        ref NativeList algorithms, ref Client client, ref MakeOptions options, out IntPtr result);

    [DllImport("webauthn.dll", CharSet = CharSet.Unicode, ExactSpelling = true)]
    private static extern int WebAuthNAuthenticatorGetAssertion(IntPtr window, string rp, ref Client client,
        ref GetOptions options, out IntPtr result);

    [DllImport("webauthn.dll")]
    private static extern void WebAuthNFreeCredentialAttestation(IntPtr result);

    [DllImport("webauthn.dll")]
    private static extern void WebAuthNFreeAssertion(IntPtr result);

    internal static bool Available()
    {
        int available;
        return WebAuthNIsUserVerifyingPlatformAuthenticatorAvailable(out available) == 0 && available != 0;
    }

    private static Client ClientData(byte[] json, NativeMemory memory)
    {
        return new Client { Version = 1, Length = (uint)json.Length, Json = memory.Pin(json), HashAlgorithm = "SHA-256" };
    }

    internal static Enrollment Enroll(IntPtr window)
    {
        using (var memory = new NativeMemory())
        {
            var rp = new Rp { Version = 1, Id = Proof.Rp, Name = "Mora synthetic approval spike" };
            var id = Proof.Nonce();
            var user = new User { Version = 1, IdLength = (uint)id.Length, Id = memory.Pin(id),
                Name = "mora-synthetic-fixture", DisplayName = "Mora synthetic fixture" };
            var algorithms = memory.List(new Algorithm { Version = 1, Type = PublicKey, Id = -7 });
            var client = ClientData(Proof.ClientData("webauthn.create", Proof.Nonce()), memory);
            var options = new MakeOptions { Version = 1, Timeout = 60000, Attachment = Platform,
                Verification = VerificationRequired, Attestation = NoAttestation };
            IntPtr result;
            int hr = WebAuthNAuthenticatorMakeCredential(window, ref rp, ref user, ref algorithms, ref client,
                ref options, out result);
            try
            {
                CheckResult(hr, result);
                var data = (MakeResult)Marshal.PtrToStructure(result, typeof(MakeResult));
                byte[] credential = Copy(data.Credential, data.CredentialLength, 1024);
                return new Enrollment(credential, ReadPublicKey(Copy(data.Auth, data.AuthLength, 4096), credential));
            }
            finally { if (result != IntPtr.Zero) WebAuthNFreeCredentialAttestation(result); }
        }
    }

    internal static Assertion Sign(IntPtr window, Enrollment enrollment, byte[] clientBytes)
    {
        using (var memory = new NativeMemory())
        {
            var client = ClientData(clientBytes, memory);
            var credential = new Credential { Version = 1, IdLength = (uint)enrollment.Credential.Length,
                Id = memory.Pin(enrollment.Credential), Type = PublicKey };
            var options = new GetOptions { Version = 1, Timeout = 60000, Allowed = memory.List(credential),
                Attachment = Platform, Verification = VerificationRequired };
            IntPtr result;
            int hr = WebAuthNAuthenticatorGetAssertion(window, Proof.Rp, ref client, ref options, out result);
            try
            {
                CheckResult(hr, result);
                var data = (GetResult)Marshal.PtrToStructure(result, typeof(GetResult));
                return new Assertion(Copy(data.Credential.Id, data.Credential.IdLength, 1024),
                    Copy(data.Auth, data.AuthLength, 4096), Copy(data.Signature, data.SignatureLength, 72));
            }
            finally { if (result != IntPtr.Zero) WebAuthNFreeAssertion(result); }
        }
    }

    private static void CheckResult(int result, IntPtr data)
    {
        if (result != 0 || data == IntPtr.Zero)
            throw new InvalidOperationException("Windows Hello did not provide proof (0x" + result.ToString("X8") + ").");
    }

    private static byte[] Copy(IntPtr source, uint length, int max)
    {
        if (source == IntPtr.Zero || length == 0 || length > max)
            throw new InvalidDataException("Unexpected native buffer size.");
        var bytes = new byte[(int)length];
        Marshal.Copy(source, bytes, 0, bytes.Length);
        return bytes;
    }

    // The spike accepts only an ES256 P-256 COSE key with the five required
    // labels. Any other credential shape leaves the experiment read-only.
    internal static byte[] ReadPublicKey(byte[] data, byte[] credential)
    {
        if (!Proof.AuthenticatorMatches(data) || data.Length < 55 || (data[32] & 0x40) == 0)
            throw new InvalidDataException("Invalid enrollment authenticator data.");
        int size = data[53] * 256 + data[54];
        if (size != credential.Length || data.Length < 55 + size)
            throw new InvalidDataException("Invalid enrollment credential length.");
        var id = new byte[size];
        Buffer.BlockCopy(data, 55, id, 0, size);
        if (!Proof.Equal(credential, id))
            throw new InvalidDataException("Enrollment credential mismatch.");

        // BCRYPT_ECCPUBLIC_BLOB (BCRYPT_ECDSA_PUBLIC_P256_MAGIC, 32 bytes each for X and Y).
        var key = new byte[72];
        byte[] header = { 0x45, 0x43, 0x53, 0x31, 32, 0, 0, 0 };
        Buffer.BlockCopy(header, 0, key, 0, header.Length);
        using (var reader = new BinaryReader(new MemoryStream(data, 55 + size, data.Length - 55 - size)))
        {
            if (reader.ReadByte() != 0xa5)
                throw new InvalidDataException("Unsupported COSE key map.");
            int seen = 0;
            for (int i = 0; i < 5; i++)
            {
                byte label = reader.ReadByte();
                int bit;
                switch (label)
                {
                    case 0x01: bit = 1; Expect(reader, 0x02); break; // kty EC2
                    case 0x03: bit = 2; Expect(reader, 0x26); break; // alg ES256
                    case 0x20: bit = 4; Expect(reader, 0x01); break; // crv P-256
                    case 0x21: bit = 8; Coordinate(reader, key, 8); break;
                    case 0x22: bit = 16; Coordinate(reader, key, 40); break;
                    default: throw new InvalidDataException("Unsupported COSE key label.");
                }
                if ((seen & bit) != 0)
                    throw new InvalidDataException("Duplicate COSE key label.");
                seen |= bit;
            }
        }
        return key;
    }

    private static void Expect(BinaryReader reader, byte expected)
    {
        if (reader.ReadByte() != expected)
            throw new InvalidDataException("Unsupported COSE key value.");
    }

    private static void Coordinate(BinaryReader reader, byte[] key, int offset)
    {
        Expect(reader, 0x58);
        Expect(reader, 32);
        byte[] coordinate = reader.ReadBytes(32);
        if (coordinate.Length != 32)
            throw new InvalidDataException("Truncated COSE coordinate.");
        Buffer.BlockCopy(coordinate, 0, key, offset, 32);
    }

    private sealed class NativeMemory : IDisposable
    {
        private readonly System.Collections.Generic.List<GCHandle> pinned =
            new System.Collections.Generic.List<GCHandle>();
        private readonly System.Collections.Generic.List<Action> cleanup =
            new System.Collections.Generic.List<Action>();

        internal IntPtr Pin(byte[] bytes)
        {
            var handle = GCHandle.Alloc(bytes, GCHandleType.Pinned);
            pinned.Add(handle);
            return handle.AddrOfPinnedObject();
        }

        internal NativeList List<T>(T value) where T : struct
        {
            IntPtr pointer = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(T)));
            try { Marshal.StructureToPtr(value, pointer, false); }
            catch { Marshal.FreeHGlobal(pointer); throw; }
            cleanup.Add(delegate {
                Marshal.DestroyStructure(pointer, typeof(T));
                Marshal.FreeHGlobal(pointer);
            });
            return new NativeList { Count = 1, Items = pointer };
        }

        public void Dispose()
        {
            foreach (var release in cleanup) release();
            foreach (var handle in pinned) handle.Free();
        }
    }
}
