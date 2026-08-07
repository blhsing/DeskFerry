package com.blhsing.deskferry.home;

import android.os.Build;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.Proxy;
import java.net.URI;
import java.net.URISyntaxException;
import java.net.URLDecoder;

import javax.net.SocketFactory;
import javax.net.ssl.SSLParameters;
import javax.net.ssl.SSLSocket;
import javax.net.ssl.SSLSocketFactory;

import okhttp3.Credentials;
import okhttp3.OkHttpClient;

final class ProxySettings {
    static final String DEFAULT = "system";

    private ProxySettings() {
    }

    static String normalize(String value) throws URISyntaxException {
        String spec = value == null ? "" : value.trim();
        if (spec.isEmpty() || "system".equalsIgnoreCase(spec)
                || "auto".equalsIgnoreCase(spec) || "env".equalsIgnoreCase(spec)) {
            return DEFAULT;
        }
        if ("direct".equalsIgnoreCase(spec)) {
            return "direct";
        }
        if (!spec.contains("://")) {
            spec = "http://" + spec;
        }
        URI uri = new URI(spec);
        String scheme = uri.getScheme() == null ? "" : uri.getScheme().toLowerCase();
        if ((!"http".equals(scheme) && !"https".equals(scheme)) || uri.getHost() == null) {
            throw new URISyntaxException(spec, "Proxy must be system, direct, or an http(s)://host:port URL");
        }
        if ((uri.getRawPath() != null && !uri.getRawPath().isEmpty())
                || uri.getRawQuery() != null || uri.getRawFragment() != null) {
            throw new URISyntaxException(spec, "Proxy URL cannot contain a path, query, or fragment");
        }
        return uri.toString();
    }

    static void apply(OkHttpClient.Builder builder, String value) throws URISyntaxException {
        String spec = normalize(value);
        if (DEFAULT.equals(spec)) {
            return;
        }
        if ("direct".equals(spec)) {
            builder.proxy(Proxy.NO_PROXY);
            return;
        }

        URI uri = new URI(spec);
        int port = uri.getPort() > 0 ? uri.getPort() : ("https".equals(uri.getScheme()) ? 443 : 80);
        builder.proxy(new Proxy(Proxy.Type.HTTP, InetSocketAddress.createUnresolved(uri.getHost(), port)));
        if ("https".equals(uri.getScheme())) {
            builder.socketFactory(new SecureProxySocketFactory((SSLSocketFactory) SSLSocketFactory.getDefault()));
        }
        if (uri.getRawUserInfo() != null) {
            String[] parts = uri.getRawUserInfo().split(":", 2);
            String username = decode(parts[0]);
            String password = parts.length > 1 ? decode(parts[1]) : "";
            builder.proxyAuthenticator((route, response) -> {
                if (response.request().header("Proxy-Authorization") != null) {
                    return null;
                }
                return response.request().newBuilder()
                        .header("Proxy-Authorization", Credentials.basic(username, password))
                        .build();
            });
        }
    }

    static String forLog(String value) {
        try {
            String spec = normalize(value);
            if (DEFAULT.equals(spec) || "direct".equals(spec)) {
                return spec;
            }
            URI uri = new URI(spec);
            int port = uri.getPort();
            return uri.getScheme() + "://" + uri.getHost() + (port > 0 ? ":" + port : "");
        } catch (URISyntaxException ex) {
            return "invalid";
        }
    }

    private static String decode(String value) {
        try {
            return URLDecoder.decode(value, "UTF-8");
        } catch (java.io.UnsupportedEncodingException impossible) {
            throw new AssertionError(impossible);
        }
    }

    private static final class SecureProxySocketFactory extends SocketFactory {
        private final SSLSocketFactory delegate;

        SecureProxySocketFactory(SSLSocketFactory delegate) {
            this.delegate = delegate;
        }

        @Override
        public java.net.Socket createSocket() throws IOException {
            SSLSocket socket = (SSLSocket) delegate.createSocket();
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                SSLParameters parameters = socket.getSSLParameters();
                parameters.setEndpointIdentificationAlgorithm("HTTPS");
                socket.setSSLParameters(parameters);
            }
            return socket;
        }

        @Override
        public java.net.Socket createSocket(String host, int port) throws IOException {
            return delegate.createSocket(host, port);
        }

        @Override
        public java.net.Socket createSocket(String host, int port, java.net.InetAddress localHost, int localPort) throws IOException {
            return delegate.createSocket(host, port, localHost, localPort);
        }

        @Override
        public java.net.Socket createSocket(java.net.InetAddress host, int port) throws IOException {
            return delegate.createSocket(host, port);
        }

        @Override
        public java.net.Socket createSocket(java.net.InetAddress address, int port,
                                            java.net.InetAddress localAddress, int localPort) throws IOException {
            return delegate.createSocket(address, port, localAddress, localPort);
        }
    }
}
