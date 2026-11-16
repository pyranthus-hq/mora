using System;
using System.Diagnostics;
using System.IO;
using System.Security.Cryptography;
using System.Text;

internal sealed class Preview
{
    internal readonly string Scope;
    internal readonly string Display;

    internal Preview(string kind, string id, string scope, string body)
    {
        Scope = scope;
        Display = "Item: " + id + "\r\nKind: " + kind + "\r\nScope: " + scope
            + "\r\nTitle: Sample decision\r\nType: fact\r\nSource: synthetic fixture"
            + "\r\nProvenance: agent-authored\r\nState: awaiting review\r\n\r\n" + body;
    }

    internal byte[] Revision { get { return Proof.Hash(Encoding.UTF8.GetBytes(Display)); } }
}

internal sealed class Decision
{
    private byte[] pending;
    private Preview preview;
    private string action;
    private long deadline;

    internal byte[] Begin(Preview shown, string selectedAction, long now)
    {
        if (selectedAction != "Keep" && selectedAction != "Discard")
            throw new ArgumentException("Unknown fixture action.");

        preview = shown;
        action = selectedAction;
        deadline = now + 60000;
        using (var frame = new MemoryStream())
        using (var writer = new BinaryWriter(frame, Encoding.UTF8))
        {
            writer.Write("mora-windows-spike-v1");
            writer.Write(Proof.Nonce());
            writer.Write(shown.Revision);
            writer.Write(shown.Scope);
            writer.Write(action);
            writer.Write(deadline);
            writer.Flush();
            pending = Proof.ClientData("webauthn.get", Proof.Hash(frame.ToArray()));
            return (byte[])pending.Clone();
        }
    }

    internal bool Commit(Preview current, string selectedAction, long now, Enrollment enrollment, Assertion proof)
    {
        byte[] expected = pending;
        Cancel();
        return expected != null && now < deadline
            && current.Scope == preview.Scope && selectedAction == action
            && Proof.Equal(current.Revision, preview.Revision)
            && Proof.Verify(enrollment, proof, expected);
    }

    internal void Cancel()
    {
        pending = null;
    }
}

internal sealed class Enrollment
{
    internal readonly byte[] Credential;
    internal readonly byte[] PublicKey;

    internal Enrollment(byte[] credential, byte[] publicKey)
    {
        Credential = credential;
        PublicKey = publicKey;
    }
}

internal sealed class Assertion
{
    internal readonly byte[] Credential;
    internal readonly byte[] AuthenticatorData;
    internal readonly byte[] Signature;

    internal Assertion(byte[] credential, byte[] authenticatorData, byte[] signature)
    {
        Credential = credential;
        AuthenticatorData = authenticatorData;
        Signature = signature;
    }
}

internal static class Proof
{
    internal const string Rp = "mora-approval-spike.invalid";
    internal static readonly Stopwatch Clock = Stopwatch.StartNew();

    internal static byte[] Hash(byte[] input)
    {
        using (var sha = SHA256.Create())
            return sha.ComputeHash(input);
    }

    internal static byte[] Nonce()
    {
        var nonce = new byte[32];
        using (var rng = RandomNumberGenerator.Create())
            rng.GetBytes(nonce);
        return nonce;
    }

    internal static byte[] ClientData(string type, byte[] challenge)
    {
        var encoded = Convert.ToBase64String(challenge).TrimEnd('=').Replace('+', '-').Replace('/', '_');
        return Encoding.UTF8.GetBytes("{\"type\":\"" + type + "\",\"challenge\":\"" + encoded
            + "\",\"origin\":\"https://" + Rp + "\",\"crossOrigin\":false}");
    }

    internal static bool Equal(byte[] left, byte[] right)
    {
        if (left == null || right == null || left.Length != right.Length)
            return false;
        int mismatch = 0;
        for (int index = 0; index < left.Length; index++)
            mismatch |= left[index] ^ right[index];
        return mismatch == 0;
    }

    internal static bool AuthenticatorMatches(byte[] data)
    {
        if (data == null || data.Length < 37 || (data[32] & 0x05) != 0x05)
            return false;
        var rpHash = new byte[32];
        Buffer.BlockCopy(data, 0, rpHash, 0, rpHash.Length);
        return Equal(rpHash, Hash(Encoding.UTF8.GetBytes(Rp)));
    }

    internal static byte[] SigningHash(byte[] data, byte[] client)
    {
        var clientHash = Hash(client);
        var signed = new byte[data.Length + clientHash.Length];
        Buffer.BlockCopy(data, 0, signed, 0, data.Length);
        Buffer.BlockCopy(clientHash, 0, signed, data.Length, clientHash.Length);
        return Hash(signed);
    }

    internal static bool Verify(Enrollment enrollment, Assertion proof, byte[] client)
    {
        if (enrollment == null || proof == null || !Equal(enrollment.Credential, proof.Credential)
            || !AuthenticatorMatches(proof.AuthenticatorData))
            return false;

        try
        {
            using (var key = CngKey.Import(enrollment.PublicKey, CngKeyBlobFormat.EccPublicBlob))
            using (var verifier = new ECDsaCng(key))
                return verifier.VerifyHash(SigningHash(proof.AuthenticatorData, client), RawSignature(proof.Signature));
        }
        catch (CryptographicException) { return false; }
        catch (InvalidDataException) { return false; }
    }

    // WebAuthn ES256 returns DER. CNG expects a 64-byte r || s signature.
    private static byte[] RawSignature(byte[] der)
    {
        if (der == null || der.Length < 8 || der.Length > 72 || der[0] != 0x30
            || der[1] != der.Length - 2)
            throw new InvalidDataException("Invalid signature sequence.");
        var raw = new byte[64];
        int offset = 2;
        for (int position = 0; position < raw.Length; position += 32)
        {
            if (offset + 2 > der.Length || der[offset++] != 2)
                throw new InvalidDataException("Invalid signature integer.");
            int count = der[offset++];
            if (count == 0 || count > 33 || offset + count > der.Length || der[offset] >= 128)
                throw new InvalidDataException("Invalid signature integer width.");
            if (count > 1 && der[offset] == 0)
            {
                if (der[offset + 1] < 128)
                    throw new InvalidDataException("Noncanonical signature integer.");
                offset++;
                count--;
            }
            if (count > 32)
                throw new InvalidDataException("Invalid signature coordinate.");
            Buffer.BlockCopy(der, offset, raw, position + 32 - count, count);
            offset += count;
        }
        if (offset != der.Length)
            throw new InvalidDataException("Trailing signature data.");
        return raw;
    }
}
