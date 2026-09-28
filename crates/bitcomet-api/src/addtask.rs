//! 添加任务的输入分类与拆分（契约 §12.1）。
//!
//! 判定顺序**固定为两步**：先看是否以 `magnet:` 开头，再看（剥掉 query/fragment 后）
//! 是否以 `.torrent` 结尾。两类都走 `POST /api/task/torrent_links/add`；
//! 其余一律走 `POST /api/task/http/add`。
//!
//! 之所以强调顺序：`magnet:?xt=...&dn=xxx.torrent` 这种链接**也**以 `.torrent` 结尾，
//! 若先判后缀就会把它当直链发去 `http/add`，服务端必然拒绝。

/// 一条输入链接应发往哪一类端点。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AddKind {
    /// `magnet:` 或 `.torrent` → `torrent_links/add`（批量）
    Torrent,
    /// 其余（`http(s)://`、`ftp://` 等）→ `http/add`（逐条）
    Http,
}

impl AddKind {
    /// 对应的服务端端点。
    pub fn endpoint(self) -> &'static str {
        match self {
            AddKind::Torrent => "/api/task/torrent_links/add",
            AddKind::Http => "/api/task/http/add",
        }
    }
}

/// 判断一条链接的类型。空串归为 `Http`（由调用方在拆分阶段过滤掉）。
pub fn classify_add_target(raw: &str) -> AddKind {
    let s = raw.trim();
    if s.to_ascii_lowercase().starts_with("magnet:") {
        return AddKind::Torrent;
    }
    // 剥掉 `?query` / `#fragment` 后再判后缀：`.torrent?token=...` 也是种子链接。
    let head = s.split(['?', '#']).next().unwrap_or(s);
    if head.to_ascii_lowercase().ends_with(".torrent") {
        return AddKind::Torrent;
    }
    AddKind::Http
}

/// 把一段（可能多行的）输入拆成若干条链接。
///
/// 分隔符：换行、逗号、分号、中文逗号/分号。**不按空格拆** ——
/// 含空格的 URL（未转义）很常见，按空格拆会把一条链接切成两条废链接。
pub fn split_add_links(input: &str) -> Vec<String> {
    input
        .split(['\n', '\r', ',', ';', '，', '；'])
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect()
}

/// 从多段输入（如 CLI 上多次 `--add-task`）汇总成一条去重后的链接列表。
pub fn collect_add_links(inputs: &[String]) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for raw in inputs {
        for link in split_add_links(raw) {
            if !out.contains(&link) {
                out.push(link);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn magnet_prefix_wins_over_torrent_suffix() {
        // 这条同时满足"magnet: 开头"和"以 .torrent 结尾"，必须先判 magnet
        let s = "magnet:?xt=urn:btih:abc&dn=Some.Movie.2020.torrent";
        assert_eq!(classify_add_target(s), AddKind::Torrent);
    }

    #[test]
    fn plain_torrent_url_is_torrent() {
        assert_eq!(
            classify_add_target("https://example.com/a/b/c.torrent"),
            AddKind::Torrent
        );
        // 大小写不敏感
        assert_eq!(
            classify_add_target("https://example.com/a.TORRENT"),
            AddKind::Torrent
        );
    }

    #[test]
    fn torrent_with_query_or_fragment_is_torrent() {
        assert_eq!(
            classify_add_target("https://ex.com/x.torrent?token=deadbeef"),
            AddKind::Torrent
        );
        assert_eq!(
            classify_add_target("https://ex.com/x.torrent#frag"),
            AddKind::Torrent
        );
    }

    #[test]
    fn http_and_ftp_are_http_kind() {
        for s in [
            "https://ex.com/file.mkv",
            "http://ex.com/file",
            "ftp://ex.com/a.iso",
            // 只是含 ".torrent" 字样，不是后缀
            "https://ex.com/x.torrent.mkv",
        ] {
            assert_eq!(classify_add_target(s), AddKind::Http, "输入 {s}");
        }
    }

    #[test]
    fn endpoint_matches_contract() {
        assert_eq!(AddKind::Torrent.endpoint(), "/api/task/torrent_links/add");
        assert_eq!(AddKind::Http.endpoint(), "/api/task/http/add");
    }

    #[test]
    fn split_handles_all_separators_and_does_not_split_on_space() {
        let input = "magnet:?xt=urn:btih:1\nhttps://a/b.torrent,\nhttps://c/d?x=1 ; ftp://e/f\n\n";
        let got = split_add_links(input);
        assert_eq!(
            got,
            vec![
                "magnet:?xt=urn:btih:1".to_string(),
                "https://a/b.torrent".to_string(),
                "https://c/d?x=1".to_string(),
                "ftp://e/f".to_string(),
            ]
        );
        // 含空格的单条链接不能被切开
        let one = split_add_links("https://ex.com/a b c.mkv");
        assert_eq!(one, vec!["https://ex.com/a b c.mkv".to_string()]);
    }

    #[test]
    fn split_drops_blank_and_keeps_chinese_separators() {
        let got = split_add_links("a，b；c;\n  ,");
        assert_eq!(got, vec!["a".to_string(), "b".to_string(), "c".to_string()]);
        assert!(split_add_links("   \n  ").is_empty());
    }

    #[test]
    fn collect_dedups_across_inputs() {
        let got = collect_add_links(&[
            "magnet:?xt=urn:btih:1".to_string(),
            "magnet:?xt=urn:btih:1\nhttps://a/b.torrent".to_string(),
        ]);
        assert_eq!(
            got,
            vec![
                "magnet:?xt=urn:btih:1".to_string(),
                "https://a/b.torrent".to_string(),
            ]
        );
    }
}
