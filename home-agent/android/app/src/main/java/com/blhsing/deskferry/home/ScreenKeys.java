package com.blhsing.deskferry.home;

import java.nio.charset.StandardCharsets;
import java.util.Locale;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

import okio.ByteString;

/**
 * Live screen key derivation (docs/screen-live-protocol.md). The relay only ever sees the room
 * proof, which is a different derivation, so it cannot recover this key.
 */
final class ScreenKeys {
    private ScreenKeys() {
    }

    /** Returns base64url(HKDF-SHA256(password, "DeskFerry screen key v1\0" + lower(trim(room)))), or "". */
    static String deriveScreenKey(String room, String password) {
        if (password == null || password.isEmpty()) {
            return "";
        }
        String normalizedRoom = room == null ? "" : room.trim().toLowerCase(Locale.ROOT);
        byte[] salt = ("DeskFerry screen key v1\u0000" + normalizedRoom).getBytes(StandardCharsets.UTF_8);
        byte[] key = hkdf(password.getBytes(StandardCharsets.UTF_8), salt,
                "DeskFerry live screen PSK".getBytes(StandardCharsets.UTF_8), 32);
        return base64Url(key);
    }

    /** Derives the screen key for the room named by a relay room URL, the same room the room proof uses. */
    static String deriveScreenKeyForRelay(String relayUrl, String password) {
        return deriveScreenKey(RelayUrls.roomToken(relayUrl, ""), password);
    }

    /** Decodes a stored screen key; returns null when it is not 32 bytes of base64url. */
    static byte[] decodeScreenKey(String value) {
        byte[] key = base64UrlDecode(value == null ? "" : value.trim());
        return key != null && key.length == 32 ? key : null;
    }

    static byte[] hkdf(byte[] secret, byte[] salt, byte[] info, int length) {
        return hkdfExpand(hkdfExtract(salt, secret), info, length);
    }

    /** RFC 5869 HKDF-Extract with SHA-256. */
    static byte[] hkdfExtract(byte[] salt, byte[] ikm) {
        byte[] key = salt == null || salt.length == 0 ? new byte[32] : salt;
        return hmacSha256(key, ikm);
    }

    /** RFC 5869 HKDF-Expand with SHA-256. */
    static byte[] hkdfExpand(byte[] prk, byte[] info, int length) {
        if (length <= 0 || length > 255 * 32) {
            throw new IllegalArgumentException("invalid HKDF length");
        }
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(prk, "HmacSHA256"));
            byte[] out = new byte[length];
            byte[] block = new byte[0];
            int offset = 0;
            for (int counter = 1; offset < length; counter++) {
                mac.update(block);
                if (info != null) mac.update(info);
                mac.update((byte) counter);
                block = mac.doFinal();
                int take = Math.min(block.length, length - offset);
                System.arraycopy(block, 0, out, offset, take);
                offset += take;
            }
            return out;
        } catch (Exception ex) {
            throw new IllegalStateException("HMAC-SHA256 is unavailable", ex);
        }
    }

    static byte[] hmacSha256(byte[] key, byte[]... parts) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(key, "HmacSHA256"));
            for (byte[] part : parts) {
                if (part != null) mac.update(part);
            }
            return mac.doFinal();
        } catch (Exception ex) {
            throw new IllegalStateException("HMAC-SHA256 is unavailable", ex);
        }
    }

    /** base64url without padding. */
    static String base64Url(byte[] value) {
        String encoded = ByteString.of(value).base64Url();
        int end = encoded.length();
        while (end > 0 && encoded.charAt(end - 1) == '=') end--;
        return encoded.substring(0, end);
    }

    /** Decodes base64url with or without padding; returns null on invalid input. */
    static byte[] base64UrlDecode(String value) {
        if (value == null) return null;
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            if (c == '+' || c == '/') return null;
        }
        ByteString decoded = ByteString.decodeBase64(value);
        return decoded == null ? null : decoded.toByteArray();
    }
}
