// pinyin-ime —— libgooglepinyin 的 stdio 协议封装，供 os-pilot-ai 在
// 本地控制台（自绘屏幕）里做中文输入。
//
// 用法: pinyin-ime <系统词典路径> <用户词典路径>
//   用户词典必须指向可写路径（libgooglepinyin 的 im_open_decoder 不接受 NULL）。
//
// 协议（一行一请求/应答，字段以 \t 分隔，UTF-8；agent 侧见 screen/ime.go）:
//   启动:         ← R\tready                （词典打开失败: R\tfail\t<原因> 后退出 1）
//   → S\t<拼音>   ← C\t<n>\t<候选0>\t…       n≤9（与数字选字键 1-9 对应），n 可为 0
//   → A\t<i>      ← A\tok                   提交候选 i（选字学习 + 清空检索状态）
//   → R           ← R\tok                   清空组合与检索状态
//   → Q                                     退出
//   无法识别:     ← E\t<原因>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "googlepinyin/pinyinime.h"

using namespace ime_pinyin;

namespace {

const size_t kMaxCands = 9;
const size_t kCandBufChars = 256;

// UTF-16（词典返回 char16）→ UTF-8
std::string utf16ToUtf8(const char16 *s) {
    std::string out;
    for (; s && *s; ++s) {
        unsigned cp = *s;
        if (cp >= 0xD800 && cp <= 0xDBFF && s[1] >= 0xDC00 && s[1] <= 0xDFFF) {
            cp = 0x10000 + ((cp - 0xD800) << 10) + (s[1] - 0xDC00);
            ++s;
        }
        if (cp < 0x80) {
            out += static_cast<char>(cp);
        } else if (cp < 0x800) {
            out += static_cast<char>(0xC0 | (cp >> 6));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        } else if (cp < 0x10000) {
            out += static_cast<char>(0xE0 | (cp >> 12));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        } else {
            out += static_cast<char>(0xF0 | (cp >> 18));
            out += static_cast<char>(0x80 | ((cp >> 12) & 0x3F));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        }
    }
    return out;
}

// 候选串不允许含协议分隔符（词典内容理论上不会出现，防御性替换）
std::string sanitize(const std::string &s) {
    std::string out = s;
    for (char &c : out) {
        if (c == '\t' || c == '\n' || c == '\r') c = ' ';
    }
    return out;
}

// 只保留小写字母与撇号（音切分）
std::string filterPinyin(const std::string &s) {
    std::string out;
    for (char c : s) {
        if ((c >= 'a' && c <= 'z') || c == '\'') out += c;
    }
    return out;
}

void respond(const std::string &line) {
    fputs(line.c_str(), stdout);
    fputc('\n', stdout);
    fflush(stdout);
}

}  // namespace

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "用法: %s <系统词典路径> <用户词典路径>\n", argv[0]);
        return 2;
    }
    if (!im_open_decoder(argv[1], argv[2])) {
        respond("R\tfail\t打开词典失败（路径或权限）");
        return 1;
    }
    im_set_max_lens(64, 32);
    respond("R\tready");

    char *line = nullptr;
    size_t cap = 0;
    while (getline(&line, &cap, stdin) != -1) {
        std::string req(line);
        while (!req.empty() && (req.back() == '\n' || req.back() == '\r')) {
            req.pop_back();
        }
        if (req.empty()) continue;

        char cmd = req[0];
        std::string arg;
        size_t tab = req.find('\t');
        if (tab != std::string::npos) arg = req.substr(tab + 1);

        switch (cmd) {
        case 'S': {
            std::string py = filterPinyin(arg);
            im_reset_search();
            if (!py.empty()) im_search(py.c_str(), py.size());
            std::vector<std::string> cands;
            char16 buf[kCandBufChars];
            while (cands.size() < kMaxCands) {
                char16 *r = im_get_candidate(cands.size(), buf, kCandBufChars);
                if (r == nullptr || r[0] == 0) break;
                cands.push_back(sanitize(utf16ToUtf8(r)));
            }
            std::string out = "C\t" + std::to_string(cands.size());
            for (const std::string &c : cands) {
                out += '\t';
                out += c;
            }
            respond(out);
            break;
        }
        case 'A': {
            long idx = strtol(arg.c_str(), nullptr, 10);
            if (idx >= 0) {
                im_choose(static_cast<size_t>(idx));
                im_flush_cache();
            }
            im_reset_search();
            respond("A\tok");
            break;
        }
        case 'R':
            im_reset_search();
            respond("R\tok");
            break;
        case 'Q':
            im_close_decoder();
            free(line);
            return 0;
        default:
            respond("E\t无法识别的命令");
        }
    }
    im_close_decoder();
    free(line);
    return 0;
}
